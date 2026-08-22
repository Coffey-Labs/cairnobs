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

	"github.com/cairnobs/cairnobs/api/authz"
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

func TestHandleSetConfigExtraFilePathsRoundTrips(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{
		ExtraFilePaths: []string{"/var/log/nginx/access.log", "/var/log/nginx/error.log"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.DesiredOverride == nil || len(got.DesiredOverride.ExtraFilePaths) != 2 {
		t.Fatalf("unexpected override: %+v", got.DesiredOverride)
	}
}

func TestHandleSetConfigRejectsRelativeExtraFilePath(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{
		ExtraFilePaths: []string{"relative/path.log"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSetConfigRejectsTooManyExtraFilePaths(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	h := newTestHandler(s)

	paths := make([]string, 21)
	for i := range paths {
		paths[i] = "/var/log/x.log"
	}
	rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{ExtraFilePaths: paths})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleSetConfigDenylistsSensitivePaths is the regression test for
// the security-audit finding that a root, unsandboxed agent plus an
// unrestricted extra_file_paths let any Editor read arbitrary files
// (e.g. /etc/shadow, SSH keys) and have them shipped into ClickHouse.
func TestHandleSetConfigDenylistsSensitivePaths(t *testing.T) {
	denied := []string{
		"/etc/shadow",
		"/etc/passwd",
		"/root/.bash_history",
		"/home/alice/.ssh/id_rsa",
		"/home/alice/.ssh/authorized_keys",
		"/proc/1/environ",
		"/etc/cairnobs-agent/client-key.pem",
		"/opt/app/../../etc/shadow",
		"/opt/app/id_ed25519",
	}
	for _, p := range denied {
		s := newFakeStore()
		s.put(Agent{TenantID: "default", Host: "web-01"})
		h := newTestHandler(s)
		rec := doRequest(t, h, "PUT", "/agents/web-01/config", ConfigOverride{ExtraFilePaths: []string{p}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400 (should be denylisted), body=%s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestHandleSetConfigExtraFilePathsRequiresAdminToAdd is the regression
// test for the audit's role-floor fix: adding/changing extra_file_paths
// needs Admin, not just Editor, since it grants the agent read access to
// a new file. Purely shrinking or clearing an existing set stays at the
// Editor floor everything else in this override uses.
func TestHandleSetConfigExtraFilePathsRequiresAdminToAdd(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	editor := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleEditor}, nil)
	admin := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleAdmin}, nil)

	rec := doRequest(t, editor, "PUT", "/agents/web-01/config", ConfigOverride{
		ExtraFilePaths: []string{"/var/log/nginx/access.log"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("editor adding a path: status = %d, want 403", rec.Code)
	}

	rec = doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{
		ExtraFilePaths: []string{"/var/log/nginx/access.log", "/var/log/nginx/error.log"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin adding paths: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// Shrinking back down to one path is a pure removal -- Editor should
	// be allowed to do this even though they couldn't have added it.
	rec = doRequest(t, editor, "PUT", "/agents/web-01/config", ConfigOverride{
		ExtraFilePaths: []string{"/var/log/nginx/access.log"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("editor removing a path: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSetConfigLogRetentionDaysRequiresOwner is the analogous
// regression test for log_retention_days -- but unlike extra_file_paths,
// there is no safe direction an Admin is allowed to move it in: setting,
// raising, lowering, and clearing all require Owner (see
// changesLogRetentionDays's doc comment for why).
func TestHandleSetConfigLogRetentionDaysRequiresOwner(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	editor := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleEditor}, nil)
	admin := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleAdmin}, nil)
	owner := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleOwner}, nil)

	days90 := 90
	rec := doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{LogRetentionDays: &days90})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin setting log_retention_days: status = %d, want 403", rec.Code)
	}

	rec = doRequest(t, owner, "PUT", "/agents/web-01/config", ConfigOverride{LogRetentionDays: &days90})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner setting log_retention_days: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.DesiredOverride == nil || got.DesiredOverride.LogRetentionDays == nil || *got.DesiredOverride.LogRetentionDays != 90 {
		t.Fatalf("stored override = %+v, want log_retention_days=90", got.DesiredOverride)
	}

	// Lowering an existing value is exactly as gated as raising it.
	days30 := 30
	rec = doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{LogRetentionDays: &days30})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin lowering log_retention_days: status = %d, want 403", rec.Code)
	}

	// Clearing it (omitting the field entirely) is also gated -- an
	// admin resending the rest of the override without this field must
	// not silently drop an owner-set floor.
	rec = doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin clearing log_retention_days: status = %d, want 403", rec.Code)
	}

	// An editor is blocked the same way an admin is -- this floor is
	// Owner-only, not Admin-or-above like extra_file_paths.
	rec = doRequest(t, editor, "PUT", "/agents/web-01/config", ConfigOverride{LogRetentionDays: &days30})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("editor setting log_retention_days: status = %d, want 403", rec.Code)
	}
}

func TestHandleSetConfigRejectsInvalidLogRetentionDays(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	owner := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleOwner}, nil)

	for _, days := range []int{0, -1, 3651} {
		d := days
		rec := doRequest(t, owner, "PUT", "/agents/web-01/config", ConfigOverride{LogRetentionDays: &d})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("log_retention_days=%d: status = %d, want 400", days, rec.Code)
		}
	}
}

// TestHandleSetConfigServiceLogRetentionDaysRequiresOwner mirrors
// TestHandleSetConfigLogRetentionDaysRequiresOwner exactly -- the
// per-service map has the same owner-only, no-safe-direction gate as
// the single host-level value.
func TestHandleSetConfigServiceLogRetentionDaysRequiresOwner(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	admin := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleAdmin}, nil)
	owner := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleOwner}, nil)

	rec := doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{
		ServiceLogRetentionDays: map[string]int{"smtp": 365},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin setting service_log_retention_days: status = %d, want 403", rec.Code)
	}

	rec = doRequest(t, owner, "PUT", "/agents/web-01/config", ConfigOverride{
		ServiceLogRetentionDays: map[string]int{"smtp": 365},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner setting service_log_retention_days: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.DesiredOverride == nil || got.DesiredOverride.ServiceLogRetentionDays["smtp"] != 365 {
		t.Fatalf("stored override = %+v, want service_log_retention_days[smtp]=365", got.DesiredOverride)
	}

	// Changing the value of an existing entry is gated the same as
	// adding a new one.
	rec = doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{
		ServiceLogRetentionDays: map[string]int{"smtp": 30},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin changing service_log_retention_days: status = %d, want 403", rec.Code)
	}

	// Clearing it (omitting the field) is gated too.
	rec = doRequest(t, admin, "PUT", "/agents/web-01/config", ConfigOverride{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin clearing service_log_retention_days: status = %d, want 403", rec.Code)
	}
}

func TestHandleSetConfigRejectsInvalidServiceLogRetentionDays(t *testing.T) {
	s := newFakeStore()
	s.put(Agent{TenantID: "default", Host: "web-01"})
	owner := NewHandler(discardLogger(), s, fakeAuthorizer{role: authz.RoleOwner}, nil)

	cases := []map[string]int{
		{"smtp": 0},
		{"smtp": -1},
		{"smtp": 3651},
		{"": 30},
	}
	for _, days := range cases {
		rec := doRequest(t, owner, "PUT", "/agents/web-01/config", ConfigOverride{ServiceLogRetentionDays: days})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("service_log_retention_days=%v: status = %d, want 400", days, rec.Code)
		}
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
