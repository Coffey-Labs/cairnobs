package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (f *fakeStore) IssueCommand(_ context.Context, tenantID, host, command, issuedBy string) (*Agent, error) {
	a, ok := f.agents[tenantID+"/"+host]
	if !ok {
		return nil, ErrNotFound
	}
	a.PendingCommand = command
	a.CommandIssuedBy = issuedBy
	cp := *a
	return &cp, nil
}

// fakeCommandLogger records LogCommand calls for assertions; nil-safe
// callers should use a nil *fakeCommandLogger the same way production
// code treats a nil CommandLogger, but tests that want to assert
// logging happened construct a real one.
type fakeCommandLogger struct {
	entries []CommandLogEntry
	err     error
}

func (f *fakeCommandLogger) LogCommand(_ context.Context, entry CommandLogEntry) error {
	f.entries = append(f.entries, entry)
	return f.err
}

func newTestHandler(s *fakeStore) *Handler {
	return NewHandler(discardLogger(), s, nil, nil)
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
	h := NewHandler(discardLogger(), s, authorizer, nil)

	interval := int64(30000)
	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{HeartbeatIntervalMS: &interval})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Viewer must not be able to edit agent config)", rec.Code)
	}
}

func TestHandleIssueCommandRoundTrips(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	logger := &fakeCommandLogger{}
	h := NewHandler(discardLogger(), s, nil, logger)

	rec := doRequest(t, h, "PUT", "/agents/web-01/command", map[string]string{"command": "restart"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.PendingCommand != "restart" {
		t.Fatalf("PendingCommand = %q, want restart", got.PendingCommand)
	}
	if len(logger.entries) != 1 || logger.entries[0].Command != "restart" || logger.entries[0].Host != "web-01" {
		t.Fatalf("unexpected audit log entries: %+v", logger.entries)
	}
}

func TestHandleIssueCommandRejectsUnknownCommand(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	rec := doRequest(t, h, "PUT", "/agents/web-01/command", map[string]string{"command": "uninstall"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (uninstall is not a supported command yet)", rec.Code)
	}
}

func TestHandleIssueCommandUnknownHostIsNotFound(t *testing.T) {
	h := newTestHandler(newFakeStore())
	rec := doRequest(t, h, "PUT", "/agents/nope/command", map[string]string{"command": "restart"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleIssueCommandFailOpenOnLoggerError is the regression test
// for CommandLogger's documented fail-open posture: an audit-log write
// failure must not turn a legitimate command issuance into an error
// response.
func TestHandleIssueCommandFailOpenOnLoggerError(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	logger := &fakeCommandLogger{err: errors.New("audit db unreachable")}
	h := NewHandler(discardLogger(), s, nil, logger)

	rec := doRequest(t, h, "PUT", "/agents/web-01/command", map[string]string{"command": "restart"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even though the audit logger failed", rec.Code)
	}
}

func TestRequireAdminRoleForCommands(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	authorizer := fakeAuthorizer{role: authz.RoleEditor}
	h := NewHandler(discardLogger(), s, authorizer, nil)

	rec := doRequest(t, h, "PUT", "/agents/web-01/command", map[string]string{"command": "restart"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Editor must not be able to issue lifecycle commands, only Admin+)", rec.Code)
	}
}

type fakeAuthorizer struct {
	role authz.Role
}

func (f fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return authz.Identity{TenantID: "default", UserID: "u1", Role: f.role}, nil
}
