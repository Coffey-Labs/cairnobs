package dashboards

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
// queryapi's SQLRunner/SearchClient. Every method except Create/Import
// takes a tenantID -- see store.go's doc comment for why.
type store interface {
	CreateDashboard(ctx context.Context, d *Dashboard) error
	ListDashboards(ctx context.Context, tenantID string) ([]Dashboard, error)
	GetDashboard(ctx context.Context, tenantID, id string) (*Dashboard, error)
	UpdateDashboard(ctx context.Context, tenantID string, d *Dashboard) error
	DeleteDashboard(ctx context.Context, tenantID, id string) error
	AddPanel(ctx context.Context, tenantID, dashboardID string, p *Panel) error
	UpdatePanel(ctx context.Context, tenantID string, p *Panel) error
	DeletePanel(ctx context.Context, tenantID, dashboardID, panelID string) error
	ImportDashboard(ctx context.Context, tenantID string, d *Dashboard) (*Dashboard, error)
}

type Handler struct {
	logger     *slog.Logger
	store      store
	authorizer authz.Authorizer
}

func NewHandler(logger *slog.Logger, store store, authorizer authz.Authorizer) *Handler {
	return &Handler{logger: logger, store: store, authorizer: authorizer}
}

// RegisterRoutes wires the RBAC minimum-role bar from
// /docs/phase-4-rbac-design.md's matrix. Note what's NOT enforced here
// yet: the matrix's "(own/granted)" qualifier for Editor create/edit/
// delete requires the dashboard_permissions/ownership lookup that
// enterprise/internal/rbacstore hasn't been built yet -- until then,
// RoleEditor is necessary but not sufficient per the matrix, and every
// Editor can act on every dashboard *within their own tenant* (tenant
// scoping itself -- a different, more basic property than the
// per-resource "(own/granted)" qualifier -- is enforced, via tenantID
// below and store.go's WHERE tenant_id = ... filtering). Tracked as
// follow-up, not silently dropped.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /dashboards", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleCreate))
	mux.HandleFunc("GET /dashboards", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleList))
	mux.HandleFunc("POST /dashboards/import", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleImport))
	mux.HandleFunc("GET /dashboards/{id}", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleGet))
	mux.HandleFunc("PUT /dashboards/{id}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleUpdate))
	mux.HandleFunc("DELETE /dashboards/{id}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleDelete))
	mux.HandleFunc("GET /dashboards/{id}/export", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleExport))
	mux.HandleFunc("POST /dashboards/{id}/panels", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleAddPanel))
	mux.HandleFunc("PUT /dashboards/{id}/panels/{panelId}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleUpdatePanel))
	mux.HandleFunc("DELETE /dashboards/{id}/panels/{panelId}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleDeletePanel))
}

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as queryapi

// tenantID resolves the tenant to scope a request's store calls to --
// from the authenticated identity RequireRole attached to the request
// context, never from a client-supplied field (a Dashboard JSON body
// can set "tenant_id" to anything; store.go's methods only ever see the
// value this function returns, not that field). Falls back to
// "default" when no identity is present (nil authorizer -- matches
// every other nil-authorizer-is-Phase-0-3-single-tenant default in this
// codebase) or when a resolved identity somehow carries no tenant (only
// RoleService identities can, per authz.Identity's doc comment, and
// RequireRole -- unlike RequireRoleOrService -- never admits RoleService,
// so this branch is a defensive fallback, not an expected path).
func (h *Handler) tenantID(r *http.Request) string {
	if id, ok := authz.IdentityFromContext(r.Context()); ok && id.TenantID != "" {
		return id.TenantID
	}
	return "default"
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	// Overrides any client-supplied tenant_id -- see tenantID's doc comment.
	d.TenantID = h.tenantID(r)
	if err := h.store.CreateDashboard(r.Context(), &d); err != nil {
		h.logger.Error("creating dashboard", "error", err)
		writeError(w, http.StatusInternalServerError, "creating dashboard failed")
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListDashboards(r.Context(), h.tenantID(r))
	if err != nil {
		h.logger.Error("listing dashboards", "error", err)
		writeError(w, http.StatusInternalServerError, "listing dashboards failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetDashboard(r.Context(), h.tenantID(r), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	// Export is the same document GET /dashboards/{id} returns -- the
	// import endpoint below consumes exactly this shape, and so does
	// `sentryctl dashboards apply`, so there's one JSON contract used
	// from every call site rather than a bespoke export format.
	h.handleGet(w, r)
}

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	imported, err := h.store.ImportDashboard(r.Context(), h.tenantID(r), &d)
	if err != nil {
		h.logger.Error("importing dashboard", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, imported)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	d.ID = r.PathValue("id")
	if err := h.store.UpdateDashboard(r.Context(), h.tenantID(r), &d); err != nil {
		h.writeStoreErr(w, err, "updating dashboard")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteDashboard(r.Context(), h.tenantID(r), r.PathValue("id")); err != nil {
		h.writeStoreErr(w, err, "deleting dashboard")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleAddPanel(w http.ResponseWriter, r *http.Request) {
	var p Panel
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := validatePanel(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.AddPanel(r.Context(), h.tenantID(r), r.PathValue("id"), &p); err != nil {
		h.writeStoreErr(w, err, "adding panel")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) handleUpdatePanel(w http.ResponseWriter, r *http.Request) {
	var p Panel
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := validatePanel(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.ID = r.PathValue("panelId")
	p.DashboardID = r.PathValue("id")
	if err := h.store.UpdatePanel(r.Context(), h.tenantID(r), &p); err != nil {
		h.writeStoreErr(w, err, "updating panel")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) handleDeletePanel(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeletePanel(r.Context(), h.tenantID(r), r.PathValue("id"), r.PathValue("panelId")); err != nil {
		h.writeStoreErr(w, err, "deleting panel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.logger.Error(action, "error", err)
	writeError(w, http.StatusInternalServerError, action+" failed")
}

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
