package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/api/authz"
)

// fakeStore enforces tenant scoping the same way store.go's real
// pgx-backed Store does (a mismatched tenantID behaves exactly like a
// missing ID -- ErrNotFound, never a distinguishable "found but wrong
// tenant" error) so handler_test.go's cross-tenant tests exercise real
// behavior, not a fake that happens to always agree.
type fakeStore struct {
	dashboards map[string]*Dashboard
	createErr  error
	importErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{dashboards: map[string]*Dashboard{}}
}

func (f *fakeStore) CreateDashboard(_ context.Context, d *Dashboard) error {
	if f.createErr != nil {
		return f.createErr
	}
	d.ID = "dash-1"
	f.dashboards[d.ID] = d
	return nil
}

func (f *fakeStore) ListDashboards(_ context.Context, tenantID string) ([]Dashboard, error) {
	var out []Dashboard
	for _, d := range f.dashboards {
		if d.TenantID == tenantID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (f *fakeStore) GetDashboard(_ context.Context, tenantID, id string) (*Dashboard, error) {
	d, ok := f.dashboards[id]
	if !ok || d.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) UpdateDashboard(_ context.Context, tenantID string, d *Dashboard) error {
	existing, ok := f.dashboards[d.ID]
	if !ok || existing.TenantID != tenantID {
		return ErrNotFound
	}
	panels := existing.Panels
	*existing = *d
	existing.TenantID = tenantID
	existing.Panels = panels
	return nil
}

func (f *fakeStore) DeleteDashboard(_ context.Context, tenantID, id string) error {
	d, ok := f.dashboards[id]
	if !ok || d.TenantID != tenantID {
		return ErrNotFound
	}
	delete(f.dashboards, id)
	return nil
}

func (f *fakeStore) AddPanel(_ context.Context, tenantID, dashboardID string, p *Panel) error {
	d, ok := f.dashboards[dashboardID]
	if !ok || d.TenantID != tenantID {
		return ErrNotFound
	}
	p.ID = "panel-1"
	p.DashboardID = dashboardID
	d.Panels = append(d.Panels, *p)
	return nil
}

func (f *fakeStore) UpdatePanel(_ context.Context, tenantID string, p *Panel) error {
	d, ok := f.dashboards[p.DashboardID]
	if !ok || d.TenantID != tenantID {
		return ErrNotFound
	}
	for i := range d.Panels {
		if d.Panels[i].ID == p.ID {
			d.Panels[i] = *p
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) DeletePanel(_ context.Context, tenantID, dashboardID, panelID string) error {
	d, ok := f.dashboards[dashboardID]
	if !ok || d.TenantID != tenantID {
		return ErrNotFound
	}
	for i := range d.Panels {
		if d.Panels[i].ID == panelID {
			d.Panels = append(d.Panels[:i], d.Panels[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) ImportDashboard(_ context.Context, tenantID string, d *Dashboard) (*Dashboard, error) {
	if f.importErr != nil {
		return nil, f.importErr
	}
	imported := *d
	imported.ID = "dash-imported"
	imported.TenantID = tenantID
	f.dashboards[imported.ID] = &imported
	return &imported, nil
}

func newTestMux(fs *fakeStore) *http.ServeMux {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs, nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// fakeAuthorizer resolves every request to a fixed identity -- used by
// the cross-tenant tests below, which need a real (non-nil) authorizer
// so tenantID(r) reads from the resolved identity instead of falling
// back to "default" for every request.
type fakeAuthorizer struct {
	identity authz.Identity
}

func (f *fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return f.identity, nil
}

// fakePermissionStore is an in-memory PermissionStore, keyed by
// dashboardID -> userID -> grant, enough to drive canEditDashboard/
// canManageGrants's "granted" path and the permission-management
// handlers without a real Postgres.
type fakePermissionStore struct {
	grants map[string]map[string]Permission
}

func newFakePermissionStore() *fakePermissionStore {
	return &fakePermissionStore{grants: map[string]map[string]Permission{}}
}

func (f *fakePermissionStore) GrantedRole(_ context.Context, dashboardID, userID string) (authz.Role, bool, error) {
	p, ok := f.grants[dashboardID][userID]
	if !ok {
		return "", false, nil
	}
	return p.Role, true, nil
}

func (f *fakePermissionStore) SetPermission(_ context.Context, dashboardID, userID string, role authz.Role, grantedBy string) error {
	if f.grants[dashboardID] == nil {
		f.grants[dashboardID] = map[string]Permission{}
	}
	f.grants[dashboardID][userID] = Permission{UserID: userID, Role: role, GrantedBy: grantedBy}
	return nil
}

func (f *fakePermissionStore) RevokePermission(_ context.Context, dashboardID, userID string) error {
	delete(f.grants[dashboardID], userID)
	return nil
}

func (f *fakePermissionStore) ListPermissions(_ context.Context, dashboardID string) ([]Permission, error) {
	var out []Permission
	for _, p := range f.grants[dashboardID] {
		out = append(out, p)
	}
	return out, nil
}

func newTestMuxWithTenant(fs *fakeStore, tenantID string) *http.ServeMux {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs,
		&fakeAuthorizer{identity: authz.Identity{TenantID: tenantID, Role: authz.RoleOwner}}, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// newTestMuxWithIdentity is newTestMuxWithTenant's more general sibling
// -- lets ownership/grant tests control Role and UserID directly (Owner
// always bypasses canEditDashboard/canManageGrants, so those tests can't
// use newTestMuxWithTenant), and optionally wires a PermissionStore.
func newTestMuxWithIdentity(fs *fakeStore, identity authz.Identity, permissions PermissionStore) *http.ServeMux {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs, &fakeAuthorizer{identity: identity}, permissions)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateDashboard(t *testing.T) {
	mux := newTestMux(newFakeStore())
	rec := doRequest(t, mux, http.MethodPost, "/dashboards", `{"name": "Overview"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ID == "" {
		t.Fatalf("expected an assigned ID, got empty")
	}
}

// TestCreateDashboardIgnoresClientSuppliedTenantID is the regression
// test for the tenant-spoofing gap found during Phase 4 task 7 (see
// /docs/security/threat-model.md's "application-layer tenant scoping"
// section): Dashboard.TenantID has a `json:"tenant_id"` tag, so a
// request body can set it to anything. The handler must always
// overwrite it from the authenticated identity, never trust the body.
func TestCreateDashboardIgnoresClientSuppliedTenantID(t *testing.T) {
	fs := newFakeStore()
	mux := newTestMuxWithTenant(fs, "acme")

	rec := doRequest(t, mux, http.MethodPost, "/dashboards", `{"name": "Overview", "tenant_id": "globex"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if fs.dashboards["dash-1"].TenantID != "acme" {
		t.Fatalf("stored TenantID = %q, want %q (the authenticated identity's tenant, not the client-supplied value)", fs.dashboards["dash-1"].TenantID, "acme")
	}
}

func TestCreateDashboardRejectsEmptyName(t *testing.T) {
	mux := newTestMux(newFakeStore())
	rec := doRequest(t, mux, http.MethodPost, "/dashboards", `{"name": ""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetDashboardNotFound(t *testing.T) {
	mux := newTestMux(newFakeStore())
	rec := doRequest(t, mux, http.MethodGet, "/dashboards/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestCrossTenantGetIsNotFound is the core adversarial case: a request
// authenticated as tenant "globex" must not be able to read a dashboard
// that belongs to tenant "acme" -- and the response must be a plain 404
// (not a 403, which would confirm the ID exists under a different
// tenant).
func TestCrossTenantGetIsNotFound(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Acme's dashboard"}
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodGet, "/dashboards/dash-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-tenant read must not succeed or leak existence via a different status)", rec.Code)
	}
}

func TestCrossTenantListDoesNotLeakOtherTenants(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Acme's dashboard"}
	fs.dashboards["dash-2"] = &Dashboard{ID: "dash-2", TenantID: "globex", Name: "Globex's dashboard"}
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodGet, "/dashboards", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 1 || got[0].ID != "dash-2" {
		t.Fatalf("expected only globex's own dashboard, got %+v", got)
	}
}

func TestCrossTenantUpdateIsNotFound(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Acme's dashboard"}
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Hijacked"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if fs.dashboards["dash-1"].Name != "Acme's dashboard" {
		t.Fatalf("cross-tenant update must not modify the row, got Name = %q", fs.dashboards["dash-1"].Name)
	}
}

func TestCrossTenantDeleteIsNotFound(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Acme's dashboard"}
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodDelete, "/dashboards/dash-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if _, ok := fs.dashboards["dash-1"]; !ok {
		t.Fatalf("cross-tenant delete must not remove the row")
	}
}

func TestCrossTenantAddPanelIsNotFound(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Acme's dashboard"}
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"query": "service=api", "viz_type": "table"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(fs.dashboards["dash-1"].Panels) != 0 {
		t.Fatalf("cross-tenant AddPanel must not attach a panel to the other tenant's dashboard")
	}
}

func TestAddPanelRejectsRawSQL(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"query": "SELECT 1", "query_language": "sql", "viz_type": "table"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddPanelRejectsInvalidVizType(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"query": "service=api", "viz_type": "pie"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddPanelSuccess(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"title": "Errors", "query": "service=api | stats count by host", "viz_type": "line", "width": 6, "height": 4}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(fs.dashboards["dash-1"].Panels) != 1 {
		t.Fatalf("expected 1 panel stored, got %d", len(fs.dashboards["dash-1"].Panels))
	}
}

func TestUpdateDashboardChangesTimeRange(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview", DefaultEarliest: "-1h", DefaultLatest: "now"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1",
		`{"name": "Overview", "default_earliest": "-24h", "default_latest": "now"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fs.dashboards["dash-1"].DefaultEarliest != "-24h" {
		t.Fatalf("expected default_earliest to be updated, got %q", fs.dashboards["dash-1"].DefaultEarliest)
	}
}

func TestUpdateDashboardNotFound(t *testing.T) {
	mux := newTestMux(newFakeStore())
	rec := doRequest(t, mux, http.MethodPut, "/dashboards/nope", `{"name": "Overview"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteDashboard(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodDelete, "/dashboards/dash-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, ok := fs.dashboards["dash-1"]; ok {
		t.Fatalf("expected dashboard to be deleted")
	}
}

func TestExportThenImportRoundTrips(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{
		ID: "dash-1", TenantID: "default", Name: "Overview",
		Panels: []Panel{{ID: "panel-1", DashboardID: "dash-1", Query: "service=api", VizType: VizTable}},
	}
	mux := newTestMux(fs)

	exportRec := doRequest(t, mux, http.MethodGet, "/dashboards/dash-1/export", "")
	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200", exportRec.Code)
	}

	importRec := doRequest(t, mux, http.MethodPost, "/dashboards/import", exportRec.Body.String())
	if importRec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201; body=%s", importRec.Code, importRec.Body.String())
	}
	var imported Dashboard
	if err := json.Unmarshal(importRec.Body.Bytes(), &imported); err != nil {
		t.Fatalf("decoding import response: %v", err)
	}
	if imported.ID == "dash-1" {
		t.Fatalf("expected import to assign a fresh ID, got the source ID back")
	}
}

// TestImportIgnoresExportedTenantID: an exported dashboard JSON file
// carries whatever tenant_id it was exported from -- importing it into
// a different tenant's session must assign it to the *importing*
// tenant, never silently move it to the tenant named in the file.
func TestImportIgnoresExportedTenantID(t *testing.T) {
	fs := newFakeStore()
	mux := newTestMuxWithTenant(fs, "globex")

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/import",
		`{"name": "Imported", "tenant_id": "acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var imported Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if imported.TenantID != "globex" {
		t.Fatalf("imported TenantID = %q, want %q (the importing identity's tenant)", imported.TenantID, "globex")
	}
}

func TestCreateDashboardStoreErrorReturns500(t *testing.T) {
	fs := newFakeStore()
	fs.createErr = errors.New("boom")
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards", `{"name": "Overview"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestServiceIdentityCannotAccessDashboards is the other half of the
// service-identity boundary (the /query half is
// api/queryapi's own tests) -- api/authz's own tests
// already prove RequireRole rejects RoleService in isolation
// (TestRequireRolePlainDoesNotAllowService); this proves it holds
// through the real dashboards handler, wired the way it's actually
// deployed, not just the middleware function in isolation. A defect
// here would mean /alerting's service token -- meant only for POST
// /query -- could also read/write dashboards, which was never the
// intent (dashboards uses plain RequireRole, not RequireRoleOrService,
// specifically to keep this door shut).
func TestServiceIdentityCannotAccessDashboards(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "default", Name: "Overview"}
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs,
		&fakeAuthorizer{identity: authz.Identity{Role: authz.RoleService}}, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, tt := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/dashboards", ""},
		{http.MethodGet, "/dashboards/dash-1", ""},
		{http.MethodPost, "/dashboards", `{"name": "New"}`},
		{http.MethodDelete, "/dashboards/dash-1", ""},
	} {
		rec := doRequest(t, mux, tt.method, tt.path, tt.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403 (RoleService must never access dashboards)", tt.method, tt.path, rec.Code)
		}
	}
}

// --- "(own/granted)" qualifier tests (Phase 4 task 5) ---

func TestEditorCannotEditOthersDashboardWithoutGrant(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Hijacked"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if fs.dashboards["dash-1"].Name != "Overview" {
		t.Fatalf("must not modify the dashboard, got Name = %q", fs.dashboards["dash-1"].Name)
	}
}

func TestEditorCanEditOwnDashboard(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-editor"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Updated"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestEditorCanEditDashboardGrantedEditorRole(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	perms := newFakePermissionStore()
	if err := perms.SetPermission(context.Background(), "dash-1", "user-editor", authz.RoleEditor, "user-owner"); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, perms)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Updated"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a granted Editor role must allow editing someone else's dashboard); body=%s", rec.Code, rec.Body.String())
	}
}

func TestEditorCannotEditDashboardGrantedViewerRoleOnly(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	perms := newFakePermissionStore()
	if err := perms.SetPermission(context.Background(), "dash-1", "user-editor", authz.RoleViewer, "user-owner"); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, perms)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Updated"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a Viewer-level grant must not allow editing -- additive-only, not an escalation)", rec.Code)
	}
}

func TestAdminCanEditAnyDashboardWithoutGrant(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-admin", Role: authz.RoleAdmin}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodDelete, "/dashboards/dash-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (Admin may act on any dashboard in its tenant); body=%s", rec.Code, rec.Body.String())
	}
}

func TestEditorCannotEditPanelsOfOthersDashboardWithoutGrant(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels", `{"query": "service=api", "viz_type": "table"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(fs.dashboards["dash-1"].Panels) != 0 {
		t.Fatalf("must not attach a panel without edit access")
	}
}

func TestNoPermissionStoreFallsBackToOwnershipOnly(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	// authorizer is real (RBAC enforced) but permissions is nil -- e.g.
	// plain api/cmd/api with ENTERPRISE_AUTH_URL set but no enterprise
	// permission service wired. Own/Admin must still work; "granted"
	// simply isn't checkable.
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, nil)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1", `{"name": "Updated"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no permission store means no 'granted' bonus, and this user isn't the creator)", rec.Code)
	}
}

// --- grant management endpoint tests ---

func TestCreatorCanManageGrants(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-creator"}
	perms := newFakePermissionStore()
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-creator", Role: authz.RoleEditor}, perms)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1/permissions/user-2", `{"role": "editor"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	role, ok, err := perms.GrantedRole(context.Background(), "dash-1", "user-2")
	if err != nil || !ok || role != authz.RoleEditor {
		t.Fatalf("GrantedRole = (%v, %v, %v), want (editor, true, nil)", role, ok, err)
	}
}

// TestGrantedEditorCannotManageGrants is the regression test for the
// matrix's distinction between "(own/granted)" (edit/delete content) and
// "(if creator)" (manage grants) -- a user who can edit only because of
// a grant must not be able to extend or re-grant that access to anyone,
// including themselves.
func TestGrantedEditorCannotManageGrants(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-owner"}
	perms := newFakePermissionStore()
	if err := perms.SetPermission(context.Background(), "dash-1", "user-editor", authz.RoleEditor, "user-owner"); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-editor", Role: authz.RoleEditor}, perms)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1/permissions/user-3", `{"role": "editor"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a granted Editor must not be able to grant access to others)", rec.Code)
	}
}

func TestSetPermissionRejectsInvalidRole(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-creator"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-creator", Role: authz.RoleEditor}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1/permissions/user-2", `{"role": "admin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (admin is not a grantable resource-level role -- see metadata/migrations/0033)", rec.Code)
	}
}

func TestRevokePermission(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-creator"}
	perms := newFakePermissionStore()
	if err := perms.SetPermission(context.Background(), "dash-1", "user-2", authz.RoleEditor, "user-creator"); err != nil {
		t.Fatalf("seeding grant: %v", err)
	}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-creator", Role: authz.RoleEditor}, perms)

	rec := doRequest(t, mux, http.MethodDelete, "/dashboards/dash-1/permissions/user-2", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := perms.GrantedRole(context.Background(), "dash-1", "user-2"); ok {
		t.Fatalf("expected the grant to be revoked")
	}
}

func TestPermissionEndpointsReturn501WithoutPermissionStore(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", TenantID: "acme", Name: "Overview", CreatedBy: "user-creator"}
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-creator", Role: authz.RoleEditor}, nil)

	rec := doRequest(t, mux, http.MethodPut, "/dashboards/dash-1/permissions/user-2", `{"role": "editor"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (no enterprise permission service wired)", rec.Code)
	}
}

// TestImportStampsImportingUserAsCreator is the regression test for the
// created_by-on-import fix: without it, an imported dashboard would keep
// the *exported* JSON's created_by, leaving the actual importer unable
// to edit their own freshly-imported copy.
func TestImportStampsImportingUserAsCreator(t *testing.T) {
	fs := newFakeStore()
	mux := newTestMuxWithIdentity(fs, authz.Identity{TenantID: "acme", UserID: "user-importer", Role: authz.RoleEditor}, newFakePermissionStore())

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/import", `{"name": "Imported", "created_by": "someone-else"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var imported Dashboard
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if imported.CreatedBy != "user-importer" {
		t.Fatalf("CreatedBy = %q, want %q (the importing identity, not the exported JSON's value)", imported.CreatedBy, "user-importer")
	}
}
