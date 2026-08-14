package queryclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuerySendsBearerServiceToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":[],"rows":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "service-token-xyz")
	if _, err := c.Query(t.Context(), "stats count", "spl", time.Second); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer service-token-xyz" {
		t.Fatalf("Authorization header = %q, want Bearer service-token-xyz", gotAuth)
	}
}

func TestQueryOmitsAuthorizationWhenNoTokenConfigured(t *testing.T) {
	var gotAuth string
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		sawHeader = r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":[],"rows":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.Query(t.Context(), "stats count", "spl", time.Second); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if sawHeader {
		t.Fatalf("expected no Authorization header when no service token is configured, got %q", gotAuth)
	}
}
