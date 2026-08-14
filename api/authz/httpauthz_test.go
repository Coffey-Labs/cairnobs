package authz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPAuthorizerForwardsCredentialsAndParsesIdentity(t *testing.T) {
	var gotCookie, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizeResponse{TenantID: "acme", UserID: "u1", Role: "editor"})
	}))
	defer srv.Close()

	a := NewHTTPAuthorizer(srv.URL)
	incoming := httptest.NewRequest(http.MethodPost, "/query", nil)
	incoming.Header.Set("Cookie", "sentry_session=abc123")
	incoming.Header.Set("Authorization", "Bearer service-token-xyz")

	identity, err := a.Authorize(incoming)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if identity.TenantID != "acme" || identity.UserID != "u1" || identity.Role != RoleEditor {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	if gotCookie != "sentry_session=abc123" {
		t.Fatalf("Cookie header not forwarded, got %q", gotCookie)
	}
	if gotAuth != "Bearer service-token-xyz" {
		t.Fatalf("Authorization header not forwarded, got %q", gotAuth)
	}
}

func TestHTTPAuthorizerNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := NewHTTPAuthorizer(srv.URL)
	_, err := a.Authorize(httptest.NewRequest(http.MethodPost, "/query", nil))
	if err == nil {
		t.Fatalf("expected an error for a 401 response from enterprise-auth")
	}
}

func TestHTTPAuthorizerDoesNotForwardUnrelatedHeaders(t *testing.T) {
	var gotXForwarded string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXForwarded = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizeResponse{Role: "viewer"})
	}))
	defer srv.Close()

	a := NewHTTPAuthorizer(srv.URL)
	incoming := httptest.NewRequest(http.MethodPost, "/query", nil)
	incoming.Header.Set("X-Forwarded-For", "1.2.3.4")

	if _, err := a.Authorize(incoming); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if gotXForwarded != "" {
		t.Fatalf("expected only Cookie/Authorization to be forwarded, but X-Forwarded-For leaked through as %q", gotXForwarded)
	}
}
