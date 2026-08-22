package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestExtractAPIFlagDefault(t *testing.T) {
	apiURL, rest := extractAPIFlag([]string{"abc123"}, func(string) string { return "" })
	if apiURL != defaultAPIURL {
		t.Fatalf("apiURL = %q, want default %q", apiURL, defaultAPIURL)
	}
	if !reflect.DeepEqual(rest, []string{"abc123"}) {
		t.Fatalf("rest = %v", rest)
	}
}

func TestExtractAPIFlagOverride(t *testing.T) {
	apiURL, rest := extractAPIFlag([]string{"--api", "http://custom:9090", "abc123"}, func(string) string { return "" })
	if apiURL != "http://custom:9090" {
		t.Fatalf("apiURL = %q", apiURL)
	}
	if !reflect.DeepEqual(rest, []string{"abc123"}) {
		t.Fatalf("rest = %v, want [abc123] (flag pair stripped)", rest)
	}
}

func TestCmdDashboardsMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsGetMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"get"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsPermissionsMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsPermissionsListMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "list"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsPermissionsListSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/dashboards/dash-1/permissions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"UserID":"user-2","Role":"editor"}]`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "list", "dash-1", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "user-2") {
		t.Fatalf("stdout = %q, want it to contain the listed grant", stdout.String())
	}
}

func TestCmdDashboardsPermissionsGrantMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "grant", "dash-1"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsPermissionsGrantInvalidRole(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "grant", "dash-1", "user-2", "owner"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "viewer") {
		t.Fatalf("stderr = %q, want it to explain the allowed roles", stderr.String())
	}
}

func TestCmdDashboardsPermissionsGrantSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/dashboards/dash-1/permissions/user-2" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"role":"editor"`) {
			t.Errorf("body = %q, want it to carry role=editor", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "grant", "dash-1", "user-2", "editor", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "granted") {
		t.Fatalf("stdout = %q, want a confirmation", stdout.String())
	}
}

func TestCmdDashboardsPermissionsGrantServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte(`{"error":"dashboard permission grants are not available on this deployment"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "grant", "dash-1", "user-2", "editor", "--api", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not available on this deployment") {
		t.Fatalf("stderr = %q, want the server's actual error message surfaced", stderr.String())
	}
}

func TestCmdDashboardsPermissionsRevokeMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "revoke", "dash-1"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsPermissionsRevokeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/dashboards/dash-1/permissions/user-2" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "revoke", "dash-1", "user-2", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Fatalf("stdout = %q, want a confirmation", stdout.String())
	}
}

func TestCmdDashboardsPermissionsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"permissions", "bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}
