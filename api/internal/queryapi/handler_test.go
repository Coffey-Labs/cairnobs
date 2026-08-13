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
)

type fakeExecutor struct {
	result *QueryResult
	err    error
	gotSQL string
}

func (f *fakeExecutor) Execute(_ context.Context, sql string) (*QueryResult, error) {
	f.gotSQL = sql
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func newTestHandler(exec queryExecutor) *Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), exec, time.Second, "*")
}

func TestHandleQuerySuccess(t *testing.T) {
	fe := &fakeExecutor{result: &QueryResult{
		Columns: []string{"host", "count"},
		Rows:    [][]any{{"h1", 3}},
	}}
	h := newTestHandler(fe)

	body := strings.NewReader(`{"sql": "SELECT host, count(*) FROM logs GROUP BY host"}`)
	req := httptest.NewRequest(http.MethodPost, "/query", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Columns) != 2 || len(got.Rows) != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if fe.gotSQL != "SELECT host, count(*) FROM logs GROUP BY host" {
		t.Fatalf("executor received unexpected SQL: %q", fe.gotSQL)
	}
}

func TestHandleQueryRejectsNonSelect(t *testing.T) {
	fe := &fakeExecutor{}
	h := newTestHandler(fe)

	body := strings.NewReader(`{"sql": "DELETE FROM logs"}`)
	req := httptest.NewRequest(http.MethodPost, "/query", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if fe.gotSQL != "" {
		t.Fatal("executor should not have been called for a rejected query")
	}
}

func TestHandleQueryRejectsInvalidJSON(t *testing.T) {
	h := newTestHandler(&fakeExecutor{})

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest(http.MethodPost, "/query", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleQueryExecutorErrorReturnsBadGateway(t *testing.T) {
	fe := &fakeExecutor{err: errors.New("boom")}
	h := newTestHandler(fe)

	body := strings.NewReader(`{"sql": "SELECT 1"}`)
	req := httptest.NewRequest(http.MethodPost, "/query", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestHandleHealthz(t *testing.T) {
	h := newTestHandler(&fakeExecutor{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newTestHandler(&fakeExecutor{})
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
