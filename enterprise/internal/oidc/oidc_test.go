package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := New(context.Background(), Config{})
	if err == nil {
		t.Fatalf("expected an error for an empty config")
	}
}

// TestNewDiscoversRealIssuer spins up a real HTTP server serving a
// minimal valid OIDC discovery document and confirms New() actually
// performs discovery against it successfully -- not just "the code
// compiles and looks plausible." Doesn't cover the full Exchange() flow
// (needs a signed JWKS/token response, real crypto scaffolding better
// suited to task 5's end-to-end auth integration tests), but discovery
// is exactly the step that would silently break on a URL-construction or
// JSON-shape mistake, so it's worth actually running.
func TestNewDiscoversRealIssuer(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"userinfo_endpoint":                     srv.URL + "/userinfo",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	})

	p, err := New(context.Background(), Config{
		IssuerURL: srv.URL, ClientID: "sentry", ClientSecret: "secret", RedirectURL: "http://localhost/callback",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.AuthCodeURL("state123") == "" {
		t.Fatalf("expected a non-empty auth code URL")
	}
}

func TestNewStateIsNonEmptyAndUnique(t *testing.T) {
	a, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	b, err := NewState()
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	if a == "" || b == "" {
		t.Fatalf("expected non-empty state values")
	}
	if a == b {
		t.Fatalf("expected two calls to NewState to produce different values")
	}
}
