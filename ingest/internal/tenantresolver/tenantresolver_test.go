package tenantresolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveTenantForwardsTokenAndParsesTenantID(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizeIngestResponse{TenantID: "acme"})
	}))
	defer srv.Close()

	res := New(srv.URL)
	tenantID, err := res.ResolveTenant(context.Background(), "real-token")
	if err != nil {
		t.Fatalf("ResolveTenant: %v", err)
	}
	if tenantID != "acme" {
		t.Fatalf("tenantID = %q, want acme", tenantID)
	}
	if gotAuth != "Bearer real-token" {
		t.Fatalf("Authorization header = %q, want Bearer real-token", gotAuth)
	}
}

func TestResolveTenantNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := New(srv.URL)
	if _, err := res.ResolveTenant(context.Background(), "bad-token"); err == nil {
		t.Fatal("expected an error for a 401 response from enterprise-auth")
	}
}

func TestResolveTenantRejectsEmptyTenantID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(authorizeIngestResponse{})
	}))
	defer srv.Close()

	res := New(srv.URL)
	if _, err := res.ResolveTenant(context.Background(), "some-token"); err == nil {
		t.Fatal("expected an error when enterprise-auth returns an empty tenant_id despite a 200")
	}
}
