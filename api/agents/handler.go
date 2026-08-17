package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sentry/sentry/api/authz"
)

// store is the narrow interface Handler depends on -- *Store (store.go)
// is the production implementation; tests use a fake, same pattern as
// dashboards.store/queryapi's SQLRunner.
type store interface {
	List(ctx context.Context, tenantID string) ([]Agent, error)
	Get(ctx context.Context, tenantID, host string) (*Agent, error)
	SetOverride(ctx context.Context, tenantID, host string, override ConfigOverride, updatedBy string) (*Agent, error)
	ClearOverride(ctx context.Context, tenantID, host string) error
	IssueCommand(ctx context.Context, tenantID, host, command, issuedBy string) (*Agent, error)
}

// CommandLogger records an issued lifecycle command into the Phase 4
// audit trail -- same nil-by-default, fail-open shape as
// aiapi.InteractionLogger and queryapi.AuditLogger: a single-tenant
// deployment with no enterprise/ configured just doesn't log these.
// enterprise/internal/audit supplies the real implementation
// (event_type = 'agent_command', see
// metadata/migrations/0039_add_agent_command_event_type.sql) -- this is
// the one entry point in this package genuinely worth logging even
// without enterprise/ wired, given "strict RBAC, full audit trail" was
// the explicit precondition for building lifecycle commands at all (see
// /docs/agent-management-design.md); it degrades gracefully rather than
// being required, matching every other optional audit hook in this
// codebase, but a real deployment should wire it.
type CommandLogger interface {
	LogCommand(ctx context.Context, entry CommandLogEntry) error
}

type CommandLogEntry struct {
	Host     string
	Command  string
	IssuedBy string
}

type Handler struct {
	logger     *slog.Logger
	store      store
	authorizer authz.Authorizer
	commands   CommandLogger
}

// commands may be nil -- see CommandLogger's doc comment.
func NewHandler(logger *slog.Logger, store store, authorizer authz.Authorizer, commands CommandLogger) *Handler {
	return &Handler{logger: logger, store: store, authorizer: authorizer, commands: commands}
}

// RegisterRoutes: viewing inventory is RoleViewer (same bar as viewing
// a dashboard); editing an agent's remote config is RoleEditor -- an
// operational-tuning action, not an admin-only one, matching the RBAC
// matrix's treatment of alert rules/notification targets rather than
// user/role management. Issuing a lifecycle command is RoleAdmin --
// stricter than config editing, matching the matrix's treatment of
// similarly consequential actions (e.g. deleting a notification
// target) rather than day-to-day tuning.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleList))
	mux.HandleFunc("GET /agents/{host}", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleGet))
	mux.HandleFunc("PUT /agents/{host}/config", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleSetConfig))
	mux.HandleFunc("DELETE /agents/{host}/config", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleClearConfig))
	mux.HandleFunc("PUT /agents/{host}/command", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleIssueCommand))
}

// tenantID mirrors dashboards.Handler.tenantID exactly -- resolved from
// the authenticated identity, never from a client-supplied field
// (there isn't one here to begin with; host alone identifies an agent
// within a tenant).
func (h *Handler) tenantID(r *http.Request) string {
	if id, ok := authz.IdentityFromContext(r.Context()); ok && id.TenantID != "" {
		return id.TenantID
	}
	return "default"
}

func (h *Handler) updatedBy(r *http.Request) string {
	if id, ok := authz.IdentityFromContext(r.Context()); ok {
		return id.UserID
	}
	return ""
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.List(r.Context(), h.tenantID(r))
	if err != nil {
		h.logger.Error("listing agents", "error", err)
		writeError(w, http.StatusInternalServerError, "listing agents failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.Get(r.Context(), h.tenantID(r), r.PathValue("host"))
	if err != nil {
		h.writeStoreErr(w, err, "getting agent")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// setConfigRequest is deliberately the same shape as ConfigOverride
// (Handler just decodes straight into it) -- every field optional,
// unset means "no override for this field." A caller changing just one
// field (e.g. only heartbeat_interval_ms) must still send the fields
// they want to KEEP as an override alongside it, since SetOverride
// replaces the whole stored override -- the web UI's edit form always
// reads the agent's current DesiredOverride first and PUTs back the
// full merged set, same pattern any other "edit form that PUTs a whole
// resource" in this codebase already uses (e.g. dashboards' PUT).
func (h *Handler) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var override ConfigOverride
	if !decodeJSON(w, r, &override) {
		return
	}
	if err := validateOverride(override); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a, err := h.store.SetOverride(r.Context(), h.tenantID(r), r.PathValue("host"), override, h.updatedBy(r))
	if err != nil {
		h.writeStoreErr(w, err, "setting agent config")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

type issueCommandRequest struct {
	Command string `json:"command"`
}

// handleIssueCommand queues a one-shot lifecycle command -- see
// Store.IssueCommand's doc comment for the delivery/clearing semantics.
// Logs to CommandLogger fail-open (a write failure is logged server-
// side and otherwise ignored, same posture as aiapi's interaction
// logging): audit-trail completeness matters, but it shouldn't be able
// to turn a legitimate restart request into a 500.
func (h *Handler) handleIssueCommand(w http.ResponseWriter, r *http.Request) {
	var req issueCommandRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validCommand(req.Command) {
		writeError(w, http.StatusBadRequest, `command must be "restart"`)
		return
	}

	host := r.PathValue("host")
	issuedBy := h.updatedBy(r)
	a, err := h.store.IssueCommand(r.Context(), h.tenantID(r), host, req.Command, issuedBy)
	if err != nil {
		h.writeStoreErr(w, err, "issuing agent command")
		return
	}

	if h.commands != nil {
		if err := h.commands.LogCommand(r.Context(), CommandLogEntry{Host: host, Command: req.Command, IssuedBy: issuedBy}); err != nil {
			h.logger.Error("logging agent command to audit trail", "host", host, "command", req.Command, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) handleClearConfig(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ClearOverride(r.Context(), h.tenantID(r), r.PathValue("host")); err != nil {
		h.writeStoreErr(w, err, "clearing agent config")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateOverride rejects the two footguns a naive remote-config-edit
// feature could otherwise ship: a batch/heartbeat interval of 0 would
// mean "flush constantly"/"heartbeat constantly," hammering ingest and
// the agent's own CPU for no operator-intended reason -- floors match
// this codebase's other real floors (alerting's own
// eval_interval_seconds >= 30, found live during the heartbeat feature
// this builds on). There is deliberately no validation here for
// tls/ingest fields, because ConfigOverride has no such fields at all
// -- ingest connection details are not a remotely-editable dimension of
// an agent's config, full stop (see /docs/agent-management-design.md's
// security boundary section).
func validateOverride(o ConfigOverride) error {
	if o.BatchMaxSize != nil && *o.BatchMaxSize < 1 {
		return errors.New("batch_max_size must be at least 1")
	}
	if o.BatchFlushIntervalMS != nil && *o.BatchFlushIntervalMS < 100 {
		return errors.New("batch_flush_interval_ms must be at least 100")
	}
	if o.HeartbeatIntervalMS != nil && *o.HeartbeatIntervalMS < 5000 {
		return errors.New("heartbeat_interval_ms must be at least 5000 (5s)")
	}
	return nil
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	h.logger.Error(action, "error", err)
	writeError(w, http.StatusInternalServerError, action+" failed")
}

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as queryapi/dashboards

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
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
