package queryapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSearchSuccess(t *testing.T) {
	id := "5754b062-ec8b-45b1-b1b8-a50f263adcd3"
	fe := &fakeExecutor{result: &QueryResult{
		Columns: []string{"message"},
		Rows:    [][]any{{"hello world"}},
	}}
	fs := &fakeSearchClient{recordIDs: []string{id}}
	h := newTestHandlerWithSearch(fe, fs)

	body := strings.NewReader(`{"query": "hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/search", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(fe.gotSQL, id) {
		t.Fatalf("expected the record_id in the generated SQL, got %q", fe.gotSQL)
	}
	if !strings.Contains(fe.gotSQL, "WHERE record_id IN") {
		t.Fatalf("expected an IN clause, got %q", fe.gotSQL)
	}

	var got QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestHandleSearchRejectsEmptyQuery(t *testing.T) {
	fe := &fakeExecutor{}
	fs := &fakeSearchClient{}
	h := newTestHandlerWithSearch(fe, fs)

	body := strings.NewReader(`{"query": "   "}`)
	req := httptest.NewRequest(http.MethodPost, "/search", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if fe.gotSQL != "" {
		t.Fatal("executor should not have been called for an empty query")
	}
}

func TestHandleSearchNoResultsReturnsEmptyNotError(t *testing.T) {
	fe := &fakeExecutor{}
	fs := &fakeSearchClient{recordIDs: nil}
	h := newTestHandlerWithSearch(fe, fs)

	body := strings.NewReader(`{"query": "nothing matches this"}`)
	req := httptest.NewRequest(http.MethodPost, "/search", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fe.gotSQL != "" {
		t.Fatal("executor should not have been called when search returns no IDs")
	}

	var got QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Rows) != 0 {
		t.Fatalf("expected empty rows, got %+v", got.Rows)
	}
}

func TestHandleSearchServiceErrorReturnsBadGateway(t *testing.T) {
	fe := &fakeExecutor{}
	fs := &fakeSearchClient{err: errors.New("search service unreachable")}
	h := newTestHandlerWithSearch(fe, fs)

	body := strings.NewReader(`{"query": "hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/search", body)
	rec := httptest.NewRecorder()

	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestRecordIDsQuerySkipsInvalidUUIDs(t *testing.T) {
	sql, err := recordIDsQuery([]string{"not-a-uuid", "5754b062-ec8b-45b1-b1b8-a50f263adcd3"})
	if err != nil {
		t.Fatalf("recordIDsQuery() error = %v", err)
	}
	if strings.Contains(sql, "not-a-uuid") {
		t.Fatalf("expected the invalid UUID to be skipped, got %q", sql)
	}
	if !strings.Contains(sql, "5754b062-ec8b-45b1-b1b8-a50f263adcd3") {
		t.Fatalf("expected the valid UUID to be included, got %q", sql)
	}
}

func TestRecordIDsQueryAllInvalidReturnsError(t *testing.T) {
	if _, err := recordIDsQuery([]string{"not-a-uuid", "also-not-one"}); err == nil {
		t.Fatal("expected an error when no IDs are valid UUIDs")
	}
}
