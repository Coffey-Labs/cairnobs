package queryapi

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
	"time"

	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/querylang/executor"
)

type fakeSQLRunner struct {
	result *executor.Result
	err    error
	gotSQL string
}

func (f *fakeSQLRunner) RunSQL(_ context.Context, sql string) (*executor.Result, error) {
	f.gotSQL = sql
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &executor.Result{Columns: []string{}, Rows: [][]any{}}, nil
}

type fakeSearchClient struct {
	recordIDs []string
	err       error
	gotQuery  string
}

func (f *fakeSearchClient) Search(_ context.Context, query string, _ uint32) ([]string, error) {
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return f.recordIDs, nil
}

func newTestHandler(sqlRunner *fakeSQLRunner, search *fakeSearchClient) *Handler {
	if search == nil {
		search = &fakeSearchClient{}
	}
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sqlRunner, search, time.Second, nil, nil)
}

type fakeAuditLogger struct {
	entries []QueryAuditEntry
	err     error
}

func (f *fakeAuditLogger) LogQuery(_ context.Context, entry QueryAuditEntry) error {
	f.entries = append(f.entries, entry)
	return f.err
}

func newTestHandlerWithAudit(sqlRunner *fakeSQLRunner, audit *fakeAuditLogger) *Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sqlRunner, &fakeSearchClient{}, time.Second, audit, nil)
}

func newTestMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func postQuery(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	newTestMux(h).ServeHTTP(rec, req)
	return rec
}

func TestHandleQuerySQLSuccess(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{
		Columns: []string{"host", "count"},
		Rows:    [][]any{{"h1", 3}},
	}}
	h := newTestHandler(sr, nil)

	rec := postQuery(t, h, `{"query": "SELECT host, count(*) FROM logs GROUP BY host"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got queryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Columns) != 2 || len(got.Rows) != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if sr.gotSQL != "SELECT host, count(*) FROM logs GROUP BY host" {
		t.Fatalf("unexpected SQL passed through: %q", sr.gotSQL)
	}
}

func TestHandleQueryPipeSyntaxSuccess(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{
		Columns: []string{"host"},
		Rows:    [][]any{{"api"}},
	}}
	h := newTestHandler(sr, nil)

	rec := postQuery(t, h, `{"query": "service=api"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sr.gotSQL, "`service` = 'api'") {
		t.Fatalf("expected compiled SQL to filter on service, got: %s", sr.gotSQL)
	}
}

func TestHandleQueryTextSearchRoutesThroughSearchClient(t *testing.T) {
	sr := &fakeSQLRunner{}
	fs := &fakeSearchClient{recordIDs: []string{"id-1"}}
	h := newTestHandler(sr, fs)

	rec := postQuery(t, h, `{"query": "message:\"connection refused\""}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fs.gotQuery != `"connection refused"` {
		t.Fatalf("search query = %q", fs.gotQuery)
	}
	if !strings.Contains(sr.gotSQL, "record_id IN ('id-1')") {
		t.Fatalf("expected the search prefilter in the generated SQL, got: %s", sr.gotSQL)
	}
}

func TestHandleQueryRejectsEmptyQuery(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	rec := postQuery(t, h, `{"query": "   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleQueryRejectsInvalidJSON(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	rec := postQuery(t, h, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleQueryRejectsCompileError(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	rec := postQuery(t, h, `{"query": "service=api | bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleQueryRejectsNonSelectSQL(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	rec := postQuery(t, h, `{"query": "DELETE FROM logs", "language": "sql"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleQueryRejectsInvalidLanguage(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	rec := postQuery(t, h, `{"query": "service=api", "language": "cobol"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleQueryExplicitLanguageOverridesAutoDetect(t *testing.T) {
	sr := &fakeSQLRunner{}
	fs := &fakeSearchClient{recordIDs: []string{"id-1"}}
	h := newTestHandler(sr, fs)

	// "select" alone would auto-detect as (nonsensical but
	// syntactically-valid-looking) SQL without the override -- the
	// override forces pipe-syntax parsing instead, where a bare word
	// with no comparator is a free-text search term.
	rec := postQuery(t, h, `{"query": "select", "language": "spl"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fs.gotQuery != "select" {
		t.Fatalf("expected 'select' to be treated as a free-text search term, got query=%q", fs.gotQuery)
	}
}

func TestHandleQueryExecutorErrorReturnsBadGateway(t *testing.T) {
	sr := &fakeSQLRunner{err: errors.New("boom")}
	h := newTestHandler(sr, nil)

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestHandleHealthz(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	newTestMux(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHandleQueryLogsAuditEntryOnSuccess(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{"host"}, Rows: [][]any{{"h1"}, {"h2"}}}}
	audit := &fakeAuditLogger{}
	h := newTestHandlerWithAudit(sr, audit)

	rec := postQuery(t, h, `{"query": "SELECT host FROM logs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	entry := audit.entries[0]
	if entry.Query != "SELECT host FROM logs" || !entry.Success || entry.RowCount != 2 || entry.Error != "" {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
}

func TestHandleQueryLogsAuditEntryOnFailure(t *testing.T) {
	sr := &fakeSQLRunner{err: errors.New("boom")}
	audit := &fakeAuditLogger{}
	h := newTestHandlerWithAudit(sr, audit)

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry even on failure, got %d", len(audit.entries))
	}
	entry := audit.entries[0]
	if entry.Success || entry.Error == "" {
		t.Fatalf("expected a failed audit entry with an error message, got %+v", entry)
	}
}

// TestHandleQueryAuditWriteFailureDoesNotFailRequest proves the
// fail-open design: a request still succeeds even when the audit
// logger itself errors -- per queryapi.AuditLogger's doc comment and
// /docs/phase-4-isolation-design.md's audit fail-open/fail-closed policy.
func TestHandleQueryAuditWriteFailureDoesNotFailRequest(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{}, Rows: [][]any{}}}
	audit := &fakeAuditLogger{err: errors.New("audit backend unreachable")}
	h := newTestHandlerWithAudit(sr, audit)

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- an audit write failure must not fail the request", rec.Code)
	}
}

func TestHandleQueryNilAuditLoggerIsNoOp(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{}, Rows: [][]any{}}}
	h := newTestHandler(sr, nil) // audit is nil here

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// fakeAuthorizer resolves every request to a fixed identity/error --
// task 5 wired authz.RequireRoleOrService into RegisterRoutes but every
// existing test above passes a nil authorizer (a deliberate no-op), so
// none of them actually exercise the wiring with a real authorizer
// present. Phase 4 task 8 (adversarial tests) closes that gap: these
// prove /query's authz boundary holds when a real Authorizer is wired
// in, not just that the middleware function works in isolation
// (authz/middleware_test.go already covers that).
type fakeAuthorizer struct {
	identity authz.Identity
	err      error
}

func (f *fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return f.identity, f.err
}

func newTestHandlerWithAuthorizer(sqlRunner *fakeSQLRunner, authorizer authz.Authorizer) *Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sqlRunner, &fakeSearchClient{}, time.Second, nil, authorizer)
}

func TestHandleQueryRejectsUnauthenticatedWhenAuthorizerConfigured(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{}, Rows: [][]any{}}}
	h := newTestHandlerWithAuthorizer(sr, &fakeAuthorizer{err: errors.New("no session")})

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleQueryAllowsViewer(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{}, Rows: [][]any{}}}
	h := newTestHandlerWithAuthorizer(sr, &fakeAuthorizer{identity: authz.Identity{TenantID: "acme", Role: authz.RoleViewer}})

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleQueryAllowsServiceIdentity is the other half of the
// alerting<->api gap's fix (/docs/phase-4-isolation-design.md) --
// /alerting's evaluator must be able to call POST /query with its
// RoleService credential even though it's not a human session.
func TestHandleQueryAllowsServiceIdentity(t *testing.T) {
	sr := &fakeSQLRunner{result: &executor.Result{Columns: []string{}, Rows: [][]any{}}}
	h := newTestHandlerWithAuthorizer(sr, &fakeAuthorizer{identity: authz.Identity{Role: authz.RoleService}})

	rec := postQuery(t, h, `{"query": "SELECT 1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- RoleService must be allowed on /query; body=%s", rec.Code, rec.Body.String())
	}
}
