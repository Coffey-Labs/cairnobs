package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentry/sentry/api/authz"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore enforces tenant scoping the same way store.go's real
// pgx-backed Store does (WHERE tenant_id = ...) -- a lookup for the
// right host under the wrong tenant behaves exactly like a missing
// host, never a distinguishable "found but wrong tenant" error, so
// handler_test.go's tenant-scoping tests exercise real behavior.
type fakeStore struct {
	agents map[string]*Agent // keyed by tenantID+"/"+host
}

func newFakeStore() *fakeStore {
	return &fakeStore{agents: map[string]*Agent{}}
}

func (f *fakeStore) put(a Agent) {
	f.agents[a.TenantID+"/"+a.Host] = &a
}

func (f *fakeStore) List(_ context.Context, tenantID string) ([]Agent, error) {
	var out []Agent
	for _, a := range f.agents {
		if a.TenantID == tenantID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeStore) Get(_ context.Context, tenantID, host string) (*Agent, error) {
	a, ok := f.agents[tenantID+"/"+host]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeStore) SetOverride(_ context.Context, tenantID, host string, override ConfigOverride, updatedBy string) (*Agent, error) {
	a, ok := f.agents[tenantID+"/"+host]
	if !ok {
		return nil, ErrNotFound
	}
	a.DesiredOverride = &override
	a.DesiredOverrideVersion = "v-test"
	a.Pending = true
	a.UpdatedBy = updatedBy
	cp := *a
	return &cp, nil
}

func (f *fakeStore) ClearOverride(_ context.Context, tenantID, host string) error {
	a, ok := f.agents[tenantID+"/"+host]
	if !ok {
		return ErrNotFound
	}
	a.DesiredOverride = nil
	a.DesiredOverrideVersion = ""
	a.Pending = false
	a.UpdatedBy = ""
	return nil
}

func newTestHandler(s *fakeStore) *Handler {
	return NewHandler(discardLogger(), s, nil)
}

func doRequest(t *testing.T, h *Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling request body: %v", err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleListScopesToTenant(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	s.put(Agent{TenantID: "acme", Host: "web-02"})
	h := newTestHandler(s)

	rec := doRequest(t, h, "GET", "/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 1 || got[0].Host != "web-01" {
		t.Fatalf("unexpected list: %+v", got)
	}
}

func TestHandleGetNotFound(t *testing.T) {
	h := newTestHandler(newFakeStore())
	rec := doRequest(t, h, "GET", "/agents/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleGetCrossTenantIsNotFound(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "acme", Host: "web-01"})
	h := newTestHandler(s) // default tenant (no authorizer/identity)

	rec := doRequest(t, h, "GET", "/agents/web-01", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (agent belongs to a different tenant)", rec.Code)
	}
}

func TestHandleSetConfigRoundTrips(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	interval := int64(30000)
	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{HeartbeatIntervalMS: &interval})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.DesiredOverride == nil || got.DesiredOverride.HeartbeatIntervalMS == nil || *got.DesiredOverride.HeartbeatIntervalMS != 30000 {
		t.Fatalf("unexpected override: %+v", got.DesiredOverride)
	}
	if !got.Pending {
		t.Fatal("expected pending=true right after setting a new override")
	}
}

func TestHandleSetConfigRejectsTooSmallHeartbeatInterval(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	tooSmall := int64(100)
	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{HeartbeatIntervalMS: &tooSmall})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSetConfigUnknownHostIsNotFound(t *testing.T) {
	h := newTestHandler(newFakeStore())
	interval := int64(30000)
	rec := doRequest(t, h, "PUT", "/agents/nope/config", ConfigOverride{HeartbeatIntervalMS: &interval})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleClearConfig(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01", Pending: true})
	h := newTestHandler(s)

	rec := doRequest(t, h, "DELETE", "/agents/web-01/config", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if s.agents["default/web-01"].Pending {
		t.Fatal("expected override to be cleared")
	}
}

func TestRequireEditorRoleForConfigWrites(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	authorizer := fakeAuthorizer{role: authz.RoleViewer}
	h := NewHandler(discardLogger(), s, authorizer)

	interval := int64(30000)
	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{HeartbeatIntervalMS: &interval})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Viewer must not be able to edit agent config)", rec.Code)
	}
}

type fakeAuthorizer struct {
	role authz.Role
}

func (f fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return authz.Identity{TenantID: "default", UserID: "u1", Role: f.role}, nil
}
