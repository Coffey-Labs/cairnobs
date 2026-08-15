package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateDashboardSendsExpectedRequest is the same "real
// httptest.Server, real HTTP round trip" pattern
// cli/cmd/sentryctl's own tests use against the same api/dashboards
// endpoints -- this client has no fake/mock mode, so its tests exercise
// real request construction and real response parsing throughout.
func TestCreateDashboardSendsExpectedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/dashboards" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		var body dashboard
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Name != "My Dashboard" {
			t.Errorf("request body Name = %q, want My Dashboard", body.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(dashboard{
			ID: "dash-1", TenantID: "acme", Name: body.Name,
			DefaultEarliest: "-1h", DefaultLatest: "now",
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "test-token")
	out, err := c.createDashboard(context.Background(), &dashboard{Name: "My Dashboard"})
	if err != nil {
		t.Fatalf("createDashboard: %v", err)
	}
	if out.ID != "dash-1" || out.TenantID != "acme" || out.DefaultEarliest != "-1h" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestGetDashboardNotFoundIsRecognizable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "dashboard not found"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !isNotFound(err) {
		t.Fatalf("isNotFound(%v) = false, want true", err)
	}
}

func TestGetDashboardServerErrorIsNotNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "dash-1")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if isNotFound(err) {
		t.Fatal("isNotFound must be false for a 500 -- only a real 404 means \"this resource is gone\"")
	}
}

func TestUpdateDashboardSendsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/dashboards/dash-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dashboard{ID: "dash-1", Name: "Renamed"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.updateDashboard(context.Background(), "dash-1", &dashboard{Name: "Renamed"})
	if err != nil {
		t.Fatalf("updateDashboard: %v", err)
	}
	if out.Name != "Renamed" {
		t.Fatalf("Name = %q, want Renamed", out.Name)
	}
}

func TestDeleteDashboardSendsToCorrectPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/dashboards/dash-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.deleteDashboard(context.Background(), "dash-1"); err != nil {
		t.Fatalf("deleteDashboard: %v", err)
	}
	if !called {
		t.Fatal("expected the server to receive a DELETE request")
	}
}

func TestDoOmitsAuthorizationHeaderWhenNoTokenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.do(context.Background(), http.MethodGet, "/dashboards", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
}

func TestApiErrorSurfacesPlainTextBodyWhenNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "dash-1")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v, want it to surface the plain-text body", err)
	}
}
