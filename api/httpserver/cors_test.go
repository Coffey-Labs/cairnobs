package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithCORSPreflight(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("POST /query", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCORS(inner, "*")

	req := httptest.NewRequest(http.MethodOptions, "/query", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestWithCORSPassesThroughNonPreflight(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCORS(inner, "*")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestWithCredentialedCORSSetsLiteralOriginAndCredentialsHeader(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /auth/memberships", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCredentialedCORS(inner, "http://localhost:3000")

	req := httptest.NewRequest(http.MethodGet, "/auth/memberships", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want http://localhost:3000", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true -- a credentialed fetch() needs this header present or the browser discards the response", got)
	}
}

func TestWithCredentialedCORSPreflight(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("POST /auth/select-tenant", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithCredentialedCORS(inner, "http://localhost:3000")

	req := httptest.NewRequest(http.MethodOptions, "/auth/select-tenant", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true on the preflight response too", got)
	}
}
