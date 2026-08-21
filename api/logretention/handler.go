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

// store is the narrow interface Handler depends on -- *Store (store.go)
// is the production implementation; tests use a fake, same pattern as
// agents.store/dashboards.store.
type store interface {
	HostsOlderThan(ctx context.Context, cutoff time.Time) ([]HostCount, error)
	CountOlderThan(ctx context.Context, cutoff time.Time, hosts []string) (uint64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time, hosts []string) error
}

// retentionFloor is the narrow interface backing the owner-only
// override check -- *AgentRetentionStore (agent_floor.go) is the
// production implementation.
type retentionFloor interface {
	RetentionDaysByHost(ctx context.Context) (map[string]int, error)
}

// maxOlderThanHours bounds the age a caller can specify -- 10 years is
// far beyond any real retention window this feature exists for, and
// exists only to reject an obviously-wrong input (e.g. a stray extra
// digit) with a clear 400 rather than silently accepting it.
const maxOlderThanHours = 10 * 365 * 24

// maxHosts caps how many host filters one request can carry -- 1000 is
// far beyond any real fleet this deployment's homelab/small-scale
// target implies, and exists only to reject a pathological/malformed
// request rather than to meaningfully restrict real usage.
const maxHosts = 1000

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
// than a stricter RoleOwner-only one.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logs/retention/hosts", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleHosts))
	mux.HandleFunc("GET /logs/retention/preview", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handlePreview))
	mux.HandleFunc("DELETE /logs/retention", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleDelete))
}

// parseOlderThanHours reads and validates the older_than_hours query
// param shared by all three routes -- a caller must ask for at least 1
// hour (an accidental empty/zero value must never mean "delete
// everything").
func parseOlderThanHours(r *http.Request) (int, bool) {
	hours, err := strconv.Atoi(r.URL.Query().Get("older_than_hours"))
	if err != nil || hours < 1 || hours > maxOlderThanHours {
		return 0, false
	}
	return hours, true
}

// parseHosts reads the repeated host query param shared by preview and
// delete -- deliberately required (at least one), never "omitted means
// every host": the whole point of this parameter existing is letting a
// caller target specific agents' logs instead of wholesale deleting
// everything, so there is no implicit "all hosts" shortcut here. GET
// /logs/retention/hosts is how a caller discovers what to pass.
// Duplicates are silently deduped; an empty host value is rejected
// outright rather than silently dropped, since a caller sending "" almost
// certainly has a client-side bug worth surfacing.
func parseHosts(r *http.Request) ([]string, bool) {
	raw := r.URL.Query()["host"]
	seen := make(map[string]struct{}, len(raw))
	var hosts []string
	for _, host := range raw {
		if host == "" {
			return nil, false
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 || len(hosts) > maxHosts {
		return nil, false
	}
	return hosts, true
}

// blockedHost is one entry in a preview/delete response's blocked_hosts
// list -- a host the caller asked about that a configured retention
// floor (api/agents.ConfigOverride.LogRetentionDays) protects from
// anyone but an owner.
type blockedHost struct {
	Host          string `json:"host"`
	ProtectedDays int    `json:"protected_days"`
}

// partitionHosts splits hosts into allowed (this caller may act on
// them) and blocked (a configured floor protects them from anyone but
// an owner, and this caller isn't one) -- role-scoped rather than a
// single all-or-nothing check, so a floor on one host never blocks
// acting on other hosts requested in the same call. An owner always
// gets everything back as allowed, no query needed.
func (h *Handler) partitionHosts(ctx context.Context, role authz.Role, hosts []string, cutoff time.Time) ([]string, []blockedHost, error) {
	if role == authz.RoleOwner {
		return hosts, nil, nil
	}
	floors, err := h.floor.RetentionDaysByHost(ctx)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	var allowed []string
	var blocked []blockedHost
	for _, host := range hosts {
		days, hasFloor := floors[host]
		if !hasFloor {
			allowed = append(allowed, host)
			continue
		}
		protectedBoundary := now.Add(-time.Duration(days) * 24 * time.Hour)
		if cutoff.After(protectedBoundary) {
			blocked = append(blocked, blockedHost{Host: host, ProtectedDays: days})
		} else {
			allowed = append(allowed, host)
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

type hostEntry struct {
	Host          string `json:"host"`
	Count         uint64 `json:"count"`
	ProtectedDays *int   `json:"protected_days,omitempty"`
}

type hostsResponse struct {
	Hosts  []hostEntry `json:"hosts"`
	Cutoff time.Time   `json:"cutoff"`
}

// handleHosts lists every host with at least one log record older than
// the requested age, annotated with any configured retention floor --
// informational for every caller regardless of role (RegisterRoutes'
// RoleAdmin floor already gates who can reach this at all); it's what
// populates the host picker a caller then selects from for preview/
// delete, not itself an action that needs partitionHosts.
func (h *Handler) handleHosts(w http.ResponseWriter, r *http.Request) {
	hours, ok := parseOlderThanHours(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	counts, err := h.store.HostsOlderThan(r.Context(), cutoff)
	if err != nil {
		h.logger.Error("listing hosts for retention preview", "error", err)
		writeError(w, http.StatusInternalServerError, "listing hosts failed")
		return
	}
	floors, err := h.floor.RetentionDaysByHost(r.Context())
	if err != nil {
		h.logger.Error("reading log retention floors", "error", err)
		writeError(w, http.StatusInternalServerError, "listing hosts failed")
		return
	}

	entries := make([]hostEntry, len(counts))
	for i, c := range counts {
		e := hostEntry{Host: c.Host, Count: c.Count}
		if days, hasFloor := floors[c.Host]; hasFloor {
			d := days
			e.ProtectedDays = &d
		}
		entries[i] = e
	}
	writeJSON(w, http.StatusOK, hostsResponse{Hosts: entries, Cutoff: cutoff})
}

type previewResponse struct {
	Count        uint64        `json:"count"`
	Cutoff       time.Time     `json:"cutoff"`
	Hosts        []string      `json:"hosts"`
	BlockedHosts []blockedHost `json:"blocked_hosts,omitempty"`
}

func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request) {
	hours, ok := parseOlderThanHours(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return
	}
	hosts, ok := parseHosts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "at least one host must be specified")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	allowed, blocked, err := h.partitionHosts(r.Context(), roleForFloorCheck(r.Context()), hosts, cutoff)
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
	writeJSON(w, http.StatusOK, previewResponse{Count: count, Cutoff: cutoff, Hosts: allowed, BlockedHosts: blocked})
}

type deleteResponse struct {
	DeletedCount uint64        `json:"deleted_count"`
	Cutoff       time.Time     `json:"cutoff"`
	DeletedHosts []string      `json:"deleted_hosts"`
	BlockedHosts []blockedHost `json:"blocked_hosts,omitempty"`
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
	hours, ok := parseOlderThanHours(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return
	}
	hosts, ok := parseHosts(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "at least one host must be specified")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	allowed, blocked, err := h.partitionHosts(r.Context(), roleForFloorCheck(r.Context()), hosts, cutoff)
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
			"deleted_count", count, "cutoff", cutoff, "hosts", allowed, "user_id", identity.UserID, "role", identity.Role)
	}

	writeJSON(w, http.StatusOK, deleteResponse{DeletedCount: count, Cutoff: cutoff, DeletedHosts: allowed, BlockedHosts: blocked})
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
