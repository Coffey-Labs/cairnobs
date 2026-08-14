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
)

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

func (f *fakeStore) ListDashboards(_ context.Context) ([]Dashboard, error) {
	var out []Dashboard
	for _, d := range f.dashboards {
		out = append(out, *d)
	}
	return out, nil
}

func (f *fakeStore) GetDashboard(_ context.Context, id string) (*Dashboard, error) {
	d, ok := f.dashboards[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) UpdateDashboard(_ context.Context, d *Dashboard) error {
	existing, ok := f.dashboards[d.ID]
	if !ok {
		return ErrNotFound
	}
	panels := existing.Panels
	*existing = *d
	existing.Panels = panels
	return nil
}

func (f *fakeStore) DeleteDashboard(_ context.Context, id string) error {
	if _, ok := f.dashboards[id]; !ok {
		return ErrNotFound
	}
	delete(f.dashboards, id)
	return nil
}

func (f *fakeStore) AddPanel(_ context.Context, dashboardID string, p *Panel) error {
	d, ok := f.dashboards[dashboardID]
	if !ok {
		return ErrNotFound
	}
	p.ID = "panel-1"
	p.DashboardID = dashboardID
	d.Panels = append(d.Panels, *p)
	return nil
}

func (f *fakeStore) UpdatePanel(_ context.Context, p *Panel) error {
	d, ok := f.dashboards[p.DashboardID]
	if !ok {
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

func (f *fakeStore) DeletePanel(_ context.Context, dashboardID, panelID string) error {
	d, ok := f.dashboards[dashboardID]
	if !ok {
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

func (f *fakeStore) ImportDashboard(_ context.Context, d *Dashboard) (*Dashboard, error) {
	if f.importErr != nil {
		return nil, f.importErr
	}
	imported := *d
	imported.ID = "dash-imported"
	f.dashboards[imported.ID] = &imported
	return &imported, nil
}

func newTestMux(fs *fakeStore) *http.ServeMux {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs)
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

func TestAddPanelRejectsRawSQL(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"query": "SELECT 1", "query_language": "sql", "viz_type": "table"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddPanelRejectsInvalidVizType(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", Name: "Overview"}
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards/dash-1/panels",
		`{"query": "service=api", "viz_type": "pie"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddPanelSuccess(t *testing.T) {
	fs := newFakeStore()
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", Name: "Overview"}
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
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", Name: "Overview", DefaultEarliest: "-1h", DefaultLatest: "now"}
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
	fs.dashboards["dash-1"] = &Dashboard{ID: "dash-1", Name: "Overview"}
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
		ID: "dash-1", Name: "Overview",
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

func TestCreateDashboardStoreErrorReturns500(t *testing.T) {
	fs := newFakeStore()
	fs.createErr = errors.New("boom")
	mux := newTestMux(fs)

	rec := doRequest(t, mux, http.MethodPost, "/dashboards", `{"name": "Overview"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
