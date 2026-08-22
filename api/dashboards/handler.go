package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cairnobs/cairnobs/api/authz"
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
	logger      *slog.Logger
	store       store
	authorizer  authz.Authorizer
	permissions PermissionStore
}

func NewHandler(logger *slog.Logger, store store, authorizer authz.Authorizer, permissions PermissionStore) *Handler {
	return &Handler{logger: logger, store: store, authorizer: authorizer, permissions: permissions}
}

// RegisterRoutes wires the RBAC minimum-role bar from
// /docs/phase-4-rbac-design.md's matrix, plus (as of Phase 4 task 5) the
// matrix's "(own/granted)" qualifier: RoleEditor is necessary but not
// sufficient for editing/deleting a dashboard or its panels -- see
// canEditDashboard. Tenant scoping itself -- a different, more basic
// property than the per-resource qualifier -- is enforced separately,
// via tenantID below and store.go's WHERE tenant_id = ... filtering.
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
	mux.HandleFunc("GET /dashboards/{id}/permissions", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleListPermissions))
	mux.HandleFunc("PUT /dashboards/{id}/permissions/{userId}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleSetPermission))
	mux.HandleFunc("DELETE /dashboards/{id}/permissions/{userId}", authz.RequireRole(h.authorizer, authz.RoleEditor, h.handleRevokePermission))
}

// canEditDashboard reports whether the request's authenticated identity
// may edit/delete d or its panels, per the matrix's Editor "(own/
// granted)" qualifier: Admin/Owner may act on any dashboard in their
// tenant (RequireRole already gated the RoleEditor floor); a plain
// Editor may act only on a dashboard they created, or one where a
// dashboard_permissions grant raises their effective role to at least
// Editor. A nil authorizer is the same no-op RequireRole already treats
// specially -- a single-tenant deployment has no identity to check
// ownership against, so this must not start rejecting Phase 0-3
// requests that never carried one. A nil permissions store (RBAC
// enforced, but no enterprise permission service wired -- e.g. plain
// api/cmd/api with ENTERPRISE_AUTH_URL set) still enforces own/Admin,
// just without the "granted" bonus.
func (h *Handler) canEditDashboard(ctx context.Context, d *Dashboard) bool {
	if h.authorizer == nil {
		return true
	}
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return false
	}
	if identity.Role.Satisfies(authz.RoleAdmin) {
		return true
	}
	if identity.UserID != "" && identity.UserID == d.CreatedBy {
		return true
	}
	if h.permissions == nil {
		return false
	}
	role, granted, err := h.permissions.GrantedRole(ctx, d.ID, identity.UserID)
	return err == nil && granted && role.Satisfies(authz.RoleEditor)
}

// canManageGrants is deliberately stricter than canEditDashboard: the
// matrix lists "manage a dashboard's per-user grants" as available to
// the creator or Admin/Owner only ("if creator", not "own/granted") --
// a user who themselves only has access *via* a grant must not be able
// to extend that access to others or re-grant themselves a persistent
// one, which a grant-based bypass here would allow.
func (h *Handler) canManageGrants(ctx context.Context, d *Dashboard) bool {
	if h.authorizer == nil {
		return true
	}
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return false
	}
	return identity.Role.Satisfies(authz.RoleAdmin) || (identity.UserID != "" && identity.UserID == d.CreatedBy)
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

// createdBy resolves the authenticated identity's UserID to stamp onto a
// newly-created dashboard, exactly like tenantID resolves TenantID --
// empty (falling back to store.go's "anonymous" default) when no
// identity is present, never from client-supplied JSON. This is what
// canEditDashboard's ownership check compares against later.
func (h *Handler) createdBy(r *http.Request) string {
	if id, ok := authz.IdentityFromContext(r.Context()); ok {
		return id.UserID
	}
	return ""
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
	// Overrides any client-supplied tenant_id/created_by -- see
	// tenantID's doc comment; same reasoning applies to created_by,
	// which canEditDashboard later trusts for the ownership check.
	d.TenantID = h.tenantID(r)
	d.CreatedBy = h.createdBy(r)
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
	// `cairnobsctl dashboards apply`, so there's one JSON contract used
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
	// Overrides any created_by the exported JSON carries -- store.go's
	// ImportDashboard trusts d.CreatedBy verbatim (unlike TenantID,
	// which it always overrides from the separate tenantID argument),
	// so without this an imported dashboard would keep its *original*
	// creator's ID, leaving the actual importer unable to edit their own
	// freshly-imported copy once canEditDashboard's ownership check
	// applies -- the same reasoning TestImportIgnoresExportedTenantID
	// already established for tenant_id extends to created_by.
	d.CreatedBy = h.createdBy(r)
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
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), d.ID)
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canEditDashboard(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.UpdateDashboard(r.Context(), h.tenantID(r), &d); err != nil {
		h.writeStoreErr(w, err, "updating dashboard")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), id)
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canEditDashboard(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.DeleteDashboard(r.Context(), h.tenantID(r), id); err != nil {
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
	dashboardID := r.PathValue("id")
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), dashboardID)
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canEditDashboard(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.AddPanel(r.Context(), h.tenantID(r), dashboardID, &p); err != nil {
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
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), p.DashboardID)
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canEditDashboard(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.UpdatePanel(r.Context(), h.tenantID(r), &p); err != nil {
		h.writeStoreErr(w, err, "updating panel")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) handleDeletePanel(w http.ResponseWriter, r *http.Request) {
	dashboardID := r.PathValue("id")
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), dashboardID)
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canEditDashboard(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := h.store.DeletePanel(r.Context(), h.tenantID(r), dashboardID, r.PathValue("panelId")); err != nil {
		h.writeStoreErr(w, err, "deleting panel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canManageGrants(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if h.permissions == nil {
		writeError(w, http.StatusNotImplemented, "dashboard permission grants are not available on this deployment")
		return
	}
	perms, err := h.permissions.ListPermissions(r.Context(), existing.ID)
	if err != nil {
		h.logger.Error("listing dashboard permissions", "error", err)
		writeError(w, http.StatusInternalServerError, "listing permissions failed")
		return
	}
	writeJSON(w, http.StatusOK, perms)
}

type setPermissionRequest struct {
	Role string `json:"role"`
}

func (h *Handler) handleSetPermission(w http.ResponseWriter, r *http.Request) {
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canManageGrants(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if h.permissions == nil {
		writeError(w, http.StatusNotImplemented, "dashboard permission grants are not available on this deployment")
		return
	}
	var body setPermissionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	role := authz.Role(body.Role)
	if !validGrantRole(role) {
		writeError(w, http.StatusBadRequest, "role must be \"viewer\" or \"editor\"")
		return
	}
	identity, _ := authz.IdentityFromContext(r.Context())
	if err := h.permissions.SetPermission(r.Context(), existing.ID, r.PathValue("userId"), role, identity.UserID); err != nil {
		h.logger.Error("setting dashboard permission", "error", err)
		writeError(w, http.StatusInternalServerError, "granting permission failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleRevokePermission(w http.ResponseWriter, r *http.Request) {
	existing, err := h.store.GetDashboard(r.Context(), h.tenantID(r), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	if !h.canManageGrants(r.Context(), existing) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if h.permissions == nil {
		writeError(w, http.StatusNotImplemented, "dashboard permission grants are not available on this deployment")
		return
	}
	if err := h.permissions.RevokePermission(r.Context(), existing.ID, r.PathValue("userId")); err != nil {
		h.logger.Error("revoking dashboard permission", "error", err)
		writeError(w, http.StatusInternalServerError, "revoking permission failed")
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
