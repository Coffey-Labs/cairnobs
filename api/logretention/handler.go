package logretention

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sentry/sentry/api/authz"
)

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as localauth/dashboards/agents

// store is the narrow interface Handler depends on -- *Store (store.go)
// is the production implementation; tests use a fake, same pattern as
// agents.store/dashboards.store.
type store interface {
	TargetsOlderThan(ctx context.Context, cutoff time.Time) ([]TargetCount, error)
	CountOlderThan(ctx context.Context, cutoff time.Time, targets []HostService) (uint64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time, targets []HostService) error
}

// retentionFloor is the narrow interface backing the owner-only
// override check -- *AgentRetentionStore (agent_floor.go) is the
// production implementation.
type retentionFloor interface {
	FloorsByHost(ctx context.Context) (map[string]HostFloor, error)
}

// maxOlderThanHours bounds the age a caller can specify -- 10 years is
// far beyond any real retention window this feature exists for, and
// exists only to reject an obviously-wrong input (e.g. a stray extra
// digit) with a clear 400 rather than silently accepting it.
const maxOlderThanHours = 10 * 365 * 24

// maxTargets caps how many (host, service) pairs one request can
// carry -- 2000 is far beyond any real fleet this deployment's
// homelab/small-scale target implies, and exists only to reject a
// pathological/malformed request rather than to meaningfully restrict
// real usage.
const maxTargets = 2000

type Handler struct {
	logger     *slog.Logger
	store      store
	floor      retentionFloor
	authorizer authz.Authorizer
}

func NewHandler(logger *slog.Logger, store store, floor retentionFloor, authorizer authz.Authorizer) *Handler {
	return &Handler{logger: logger, store: store, floor: floor, authorizer: authorizer}
}

// RegisterRoutes: all three routes are RoleAdmin -- RoleOwner satisfies
// it too (Role.Satisfies is a floor, not an exact match), matching the
// "owner and admin" requirement this feature shipped for. Permanently
// deleting log data is at least as consequential as the RBAC matrix's
// other RoleAdmin-floor actions (e.g. issuing an agent restart
// command, api/agents/handler.go), so it gets the same floor rather
// than a stricter RoleOwner-only one. preview/delete are POST, not
// GET/DELETE-with-query-params, because a request now carries a list
// of (host, service) pairs -- a JSON body is the natural shape for
// that, not a repeated compound query param.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logs/retention/hosts", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleHosts))
	mux.HandleFunc("POST /logs/retention/preview", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handlePreview))
	mux.HandleFunc("POST /logs/retention/delete", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleDelete))
}

// parseOlderThanHours reads and validates the older_than_hours query
// param GET /logs/retention/hosts uses -- preview/delete take the same
// value from their JSON body instead (see deletionRequest).
func parseOlderThanHours(r *http.Request) (int, bool) {
	hours, err := strconv.Atoi(r.URL.Query().Get("older_than_hours"))
	if err != nil || hours < 1 || hours > maxOlderThanHours {
		return 0, false
	}
	return hours, true
}

// deletionRequest is the JSON body preview and delete both take --
// targets is deliberately required and never empty: there is no
// "omitted means every host/service" shortcut anywhere in this
// package, matching the feature's whole point (select what you mean to
// act on, never wholesale-delete by accident).
type deletionRequest struct {
	OlderThanHours int           `json:"older_than_hours"`
	Targets        []HostService `json:"targets"`
}

// parseTargets validates and normalizes a decoded request's targets:
// deduped, every host and service non-empty, count within maxTargets.
func parseTargets(targets []HostService) ([]HostService, bool) {
	seen := make(map[HostService]struct{}, len(targets))
	var out []HostService
	for _, t := range targets {
		if t.Host == "" || t.Service == "" {
			return nil, false
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 || len(out) > maxTargets {
		return nil, false
	}
	return out, true
}

func decodeDeletionRequest(w http.ResponseWriter, r *http.Request) (deletionRequest, bool) {
	var req deletionRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return deletionRequest{}, false
	}
	if req.OlderThanHours < 1 || req.OlderThanHours > maxOlderThanHours {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return deletionRequest{}, false
	}
	targets, ok := parseTargets(req.Targets)
	if !ok {
		writeError(w, http.StatusBadRequest, "targets must name at least one non-empty host/service pair")
		return deletionRequest{}, false
	}
	req.Targets = targets
	return req, true
}

// blockedTarget is one entry in a preview/delete response's
// blocked_targets list -- a (host, service) the caller asked about
// that a configured retention floor (api/agents.ConfigOverride.
// LogRetentionDays / ServiceLogRetentionDays) protects from anyone but
// an owner.
type blockedTarget struct {
	Host          string `json:"host"`
	Service       string `json:"service"`
	ProtectedDays int    `json:"protected_days"`
}

// partitionTargets splits targets into allowed (this caller may act on
// them) and blocked (a configured floor protects them from anyone but
// an owner, and this caller isn't one) -- per-target rather than a
// single all-or-nothing check, so a floor on one target never blocks
// acting on other targets requested in the same call. An owner always
// gets everything back as allowed, no query needed.
func (h *Handler) partitionTargets(ctx context.Context, role authz.Role, targets []HostService, cutoff time.Time) ([]HostService, []blockedTarget, error) {
	if role == authz.RoleOwner {
		return targets, nil, nil
	}
	floors, err := h.floor.FloorsByHost(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	var allowed []HostService
	var blocked []blockedTarget
	for _, t := range targets {
		days, hasFloor := floors[t.Host].Effective(t.Service)
		if !hasFloor {
			allowed = append(allowed, t)
			continue
		}
		protectedBoundary := now.Add(-time.Duration(days) * 24 * time.Hour)
		if cutoff.After(protectedBoundary) {
			blocked = append(blocked, blockedTarget{Host: t.Host, Service: t.Service, ProtectedDays: days})
		} else {
			allowed = append(allowed, t)
		}
	}
	return allowed, blocked, nil
}

// roleForFloorCheck returns the identity's role, or RoleOwner (i.e.
// "bypass the floor entirely") when no identity resolved at all -- a
// nil authorizer means no RBAC is configured (Phase 0-3 default-open),
// and this feature's owner-only override must stay a no-op in that
// case too, consistent with every other RBAC check in this codebase.
func roleForFloorCheck(ctx context.Context) authz.Role {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return authz.RoleOwner
	}
	return identity.Role
}

type serviceEntry struct {
	Service       string `json:"service"`
	Count         uint64 `json:"count"`
	ProtectedDays *int   `json:"protected_days,omitempty"`
}

type hostEntry struct {
	Host          string         `json:"host"`
	ProtectedDays *int           `json:"protected_days,omitempty"`
	Services      []serviceEntry `json:"services"`
}

type hostsResponse struct {
	Hosts  []hostEntry `json:"hosts"`
	Cutoff time.Time   `json:"cutoff"`
}

// handleHosts lists every (host, service) pair with at least one log
// record older than the requested age, grouped by host and annotated
// with any configured retention floor -- informational for every
// caller regardless of role (RegisterRoutes' RoleAdmin floor already
// gates who can reach this at all); it's what populates the picker a
// caller then selects from for preview/delete, not itself an action
// that needs partitionTargets.
func (h *Handler) handleHosts(w http.ResponseWriter, r *http.Request) {
	hours, ok := parseOlderThanHours(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	counts, err := h.store.TargetsOlderThan(r.Context(), cutoff)
	if err != nil {
		h.logger.Error("listing targets for retention preview", "error", err)
		writeError(w, http.StatusInternalServerError, "listing hosts failed")
		return
	}
	floors, err := h.floor.FloorsByHost(r.Context())
	if err != nil {
		h.logger.Error("reading log retention floors", "error", err)
		writeError(w, http.StatusInternalServerError, "listing hosts failed")
		return
	}

	// counts is ordered by host (Store.TargetsOlderThan), so contiguous
	// rows for the same host can be grouped in one pass.
	var hosts []hostEntry
	for _, c := range counts {
		hf := floors[c.Host]
		if len(hosts) == 0 || hosts[len(hosts)-1].Host != c.Host {
			he := hostEntry{Host: c.Host}
			if hf.DefaultDays != nil {
				d := *hf.DefaultDays
				he.ProtectedDays = &d
			}
			hosts = append(hosts, he)
		}
		se := serviceEntry{Service: c.Service, Count: c.Count}
		if days, hasFloor := hf.Effective(c.Service); hasFloor {
			se.ProtectedDays = &days
		}
		hosts[len(hosts)-1].Services = append(hosts[len(hosts)-1].Services, se)
	}

	writeJSON(w, http.StatusOK, hostsResponse{Hosts: hosts, Cutoff: cutoff})
}

type previewResponse struct {
	Count          uint64          `json:"count"`
	Cutoff         time.Time       `json:"cutoff"`
	Targets        []HostService   `json:"targets"`
	BlockedTargets []blockedTarget `json:"blocked_targets,omitempty"`
}

func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeDeletionRequest(w, r)
	if !ok {
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(req.OlderThanHours) * time.Hour)

	allowed, blocked, err := h.partitionTargets(r.Context(), roleForFloorCheck(r.Context()), req.Targets, cutoff)
	if err != nil {
		h.logger.Error("checking log retention floor", "error", err)
		writeError(w, http.StatusInternalServerError, "checking retention policy failed")
		return
	}

	var count uint64
	if len(allowed) > 0 {
		count, err = h.store.CountOlderThan(r.Context(), cutoff, allowed)
		if err != nil {
			h.logger.Error("counting logs for retention preview", "error", err)
			writeError(w, http.StatusInternalServerError, "counting logs failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, previewResponse{Count: count, Cutoff: cutoff, Targets: allowed, BlockedTargets: blocked})
}

type deleteResponse struct {
	DeletedCount   uint64          `json:"deleted_count"`
	Cutoff         time.Time       `json:"cutoff"`
	DeletedTargets []HostService   `json:"deleted_targets"`
	BlockedTargets []blockedTarget `json:"blocked_targets,omitempty"`
}

// handleDelete counts immediately before deleting so the response can
// report how many records were actually removed -- ClickHouse's ALTER
// TABLE DELETE mutation itself reports no row count. A handful of
// records landing between this count and the delete would still be
// older than the fixed cutoff by the time they land, so the delete
// catches them too even though this count didn't -- an acceptable,
// disclosed margin for an admin-facing summary number, not something
// anything downstream depends on for correctness.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeDeletionRequest(w, r)
	if !ok {
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(req.OlderThanHours) * time.Hour)

	allowed, blocked, err := h.partitionTargets(r.Context(), roleForFloorCheck(r.Context()), req.Targets, cutoff)
	if err != nil {
		h.logger.Error("checking log retention floor", "error", err)
		writeError(w, http.StatusInternalServerError, "checking retention policy failed")
		return
	}

	var count uint64
	if len(allowed) > 0 {
		count, err = h.store.CountOlderThan(r.Context(), cutoff, allowed)
		if err != nil {
			h.logger.Error("counting logs before retention delete", "error", err)
			writeError(w, http.StatusInternalServerError, "counting logs failed")
			return
		}

		if err := h.store.DeleteOlderThan(r.Context(), cutoff, allowed); err != nil {
			h.logger.Error("deleting logs by retention age", "error", err)
			writeError(w, http.StatusInternalServerError, "deleting logs failed")
			return
		}

		identity, _ := authz.IdentityFromContext(r.Context())
		h.logger.Info("logs deleted by retention age",
			"deleted_count", count, "cutoff", cutoff, "targets", allowed, "user_id", identity.UserID, "role", identity.Role)
	}

	writeJSON(w, http.StatusOK, deleteResponse{DeletedCount: count, Cutoff: cutoff, DeletedTargets: allowed, BlockedTargets: blocked})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
