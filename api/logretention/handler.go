package logretention

import (
	"context"
	"encoding/json"
	"fmt"
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
	CountOlderThan(ctx context.Context, cutoff time.Time) (uint64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) error
}

// retentionFloor is the narrow interface backing the owner-only
// override check -- *AgentRetentionStore (agent_floor.go) is the
// production implementation.
type retentionFloor interface {
	MaxRetentionDays(ctx context.Context) (int, bool, error)
}

// maxOlderThanHours bounds the age a caller can specify -- 10 years is
// far beyond any real retention window this feature exists for, and
// exists only to reject an obviously-wrong input (e.g. a stray extra
// digit) with a clear 400 rather than silently accepting it.
const maxOlderThanHours = 10 * 365 * 24

type Handler struct {
	logger     *slog.Logger
	store      store
	floor      retentionFloor
	authorizer authz.Authorizer
}

func NewHandler(logger *slog.Logger, store store, floor retentionFloor, authorizer authz.Authorizer) *Handler {
	return &Handler{logger: logger, store: store, floor: floor, authorizer: authorizer}
}

// RegisterRoutes: both routes are RoleAdmin -- RoleOwner satisfies it
// too (Role.Satisfies is a floor, not an exact match), matching the
// "owner and admin" requirement this feature shipped for. Permanently
// deleting log data is at least as consequential as the RBAC matrix's
// other RoleAdmin-floor actions (e.g. issuing an agent restart
// command, api/agents/handler.go), so it gets the same floor rather
// than a stricter RoleOwner-only one.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logs/retention/preview", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handlePreview))
	mux.HandleFunc("DELETE /logs/retention", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleDelete))
}

// parseOlderThanHours reads and validates the older_than_hours query
// param shared by both routes -- a caller must ask for at least 1 hour
// (an accidental empty/zero value must never mean "delete everything").
func parseOlderThanHours(r *http.Request) (int, bool) {
	hours, err := strconv.Atoi(r.URL.Query().Get("older_than_hours"))
	if err != nil || hours < 1 || hours > maxOlderThanHours {
		return 0, false
	}
	return hours, true
}

// checkRetentionFloor enforces api/agents.ConfigOverride.LogRetentionDays
// as a hard floor against anyone but an owner: if any agent has a
// configured retention, the largest one across all agents is the
// earliest boundary a non-owner may delete up to. An owner always
// bypasses this (identity.Role == RoleOwner short-circuits before ever
// querying the floor) -- "owner and admin" gates the routes themselves
// (RegisterRoutes), but this narrows what admin specifically can do
// once inside them, the same shape handleSetConfig's own
// log_retention_days gate uses on the agents side. A nil identity (no
// authorizer configured at all, Phase 0-3 default-open) skips this
// too, consistent with every other RBAC check in this codebase being a
// no-op when there's no RBAC to begin with.
func (h *Handler) checkRetentionFloor(ctx context.Context, cutoff time.Time) (string, error) {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok || identity.Role == authz.RoleOwner {
		return "", nil
	}
	maxDays, hasFloor, err := h.floor.MaxRetentionDays(ctx)
	if err != nil {
		return "", err
	}
	if !hasFloor {
		return "", nil
	}
	protectedBoundary := time.Now().UTC().Add(-time.Duration(maxDays) * 24 * time.Hour)
	if cutoff.After(protectedBoundary) {
		return fmt.Sprintf("a configured log retention policy protects logs newer than %d days; only an owner can override this", maxDays), nil
	}
	return "", nil
}

type previewResponse struct {
	Count  uint64    `json:"count"`
	Cutoff time.Time `json:"cutoff"`
}

func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request) {
	hours, ok := parseOlderThanHours(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "older_than_hours must be a positive integer")
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	if msg, err := h.checkRetentionFloor(r.Context(), cutoff); err != nil {
		h.logger.Error("checking log retention floor", "error", err)
		writeError(w, http.StatusInternalServerError, "checking retention policy failed")
		return
	} else if msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}

	count, err := h.store.CountOlderThan(r.Context(), cutoff)
	if err != nil {
		h.logger.Error("counting logs for retention preview", "error", err)
		writeError(w, http.StatusInternalServerError, "counting logs failed")
		return
	}
	writeJSON(w, http.StatusOK, previewResponse{Count: count, Cutoff: cutoff})
}

type deleteResponse struct {
	DeletedCount uint64    `json:"deleted_count"`
	Cutoff       time.Time `json:"cutoff"`
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
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	if msg, err := h.checkRetentionFloor(r.Context(), cutoff); err != nil {
		h.logger.Error("checking log retention floor", "error", err)
		writeError(w, http.StatusInternalServerError, "checking retention policy failed")
		return
	} else if msg != "" {
		writeError(w, http.StatusForbidden, msg)
		return
	}

	count, err := h.store.CountOlderThan(r.Context(), cutoff)
	if err != nil {
		h.logger.Error("counting logs before retention delete", "error", err)
		writeError(w, http.StatusInternalServerError, "counting logs failed")
		return
	}

	if err := h.store.DeleteOlderThan(r.Context(), cutoff); err != nil {
		h.logger.Error("deleting logs by retention age", "error", err)
		writeError(w, http.StatusInternalServerError, "deleting logs failed")
		return
	}

	identity, _ := authz.IdentityFromContext(r.Context())
	h.logger.Info("logs deleted by retention age",
		"deleted_count", count, "cutoff", cutoff, "user_id", identity.UserID, "role", identity.Role)

	writeJSON(w, http.StatusOK, deleteResponse{DeletedCount: count, Cutoff: cutoff})
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
