package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestResolveTokenFromEnv(t *testing.T) {
	env := func(k string) string {
		if k == "CAIRNOBSCTL_TOKEN" {
			return "secret-token"
		}
		return ""
	}
	if got := resolveToken(env); got != "secret-token" {
		t.Errorf("got %q, want %q", got, "secret-token")
	}
}

func TestHTTPGetJSONForwardsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := httpGetJSON(srv.URL, "/thing", "my-token", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuth != "Bearer my-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer my-token")
	}
}

func TestHTTPGetJSONOmitsAuthorizationWhenNoToken(t *testing.T) {
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("Authorization") != ""
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := httpGetJSON(srv.URL, "/thing", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if sawHeader {
		t.Fatalf("expected no Authorization header when no token is configured")
	}
}

func TestHTTPPostFileJSONForwardsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	f, err := os.CreateTemp(t.TempDir(), "payload-*.json")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(`{"name":"test"}`); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	f.Close()

	var stdout, stderr bytes.Buffer
	code := httpPostFileJSON(srv.URL, "/thing", "my-token", f.Name(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuth != "Bearer my-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer my-token")
	}
}

func TestCmdPingForwardsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("CAIRNOBSCTL_TOKEN", "ping-token")
	var stdout, stderr bytes.Buffer
	code := cmdPing([]string{"--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuth != "Bearer ping-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer ping-token")
	}
}

func TestCmdQueryForwardsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"columns":[],"rows":[]}`))
	}))
	defer srv.Close()

	t.Setenv("CAIRNOBSCTL_TOKEN", "query-token")
	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--api", srv.URL, "service=api"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if gotAuth != "Bearer query-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer query-token")
	}
}
