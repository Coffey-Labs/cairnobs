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

	"github.com/sentry/sentry/api/internal/querylang/executor"
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
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sqlRunner, search, time.Second, "*")
}

func postQuery(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
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

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newTestHandler(&fakeSQLRunner{}, nil)
	req := httptest.NewRequest(http.MethodOptions, "/query", nil)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
}
