package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePingArgsDefault(t *testing.T) {
	env := func(string) string { return "" }
	if got := parsePingArgs(nil, env); got != defaultAPIURL {
		t.Errorf("got %q, want %q", got, defaultAPIURL)
	}
}

func TestParsePingArgsFromEnv(t *testing.T) {
	env := func(k string) string {
		if k == "SENTRYCTL_API_URL" {
			return "http://env-host:1234"
		}
		return ""
	}
	if got := parsePingArgs(nil, env); got != "http://env-host:1234" {
		t.Errorf("got %q, want env value", got)
	}
}

func TestParsePingArgsFlagOverridesEnv(t *testing.T) {
	env := func(k string) string {
		if k == "SENTRYCTL_API_URL" {
			return "http://env-host:1234"
		}
		return ""
	}
	got := parsePingArgs([]string{"--api", "http://flag-host:5678"}, env)
	if got != "http://flag-host:5678" {
		t.Errorf("got %q, want flag value", got)
	}
}

func TestCmdPingSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdPing([]string{"--api", srv.URL}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "ok" {
		t.Fatalf("stdout = %q, want ok", stdout.String())
	}
}

func TestCmdPingNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdPing([]string{"--api", srv.URL}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "503") {
		t.Fatalf("stderr = %q, want it to mention the status code", stderr.String())
	}
}

func TestCmdPingUnreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdPing([]string{"--api", "http://127.0.0.1:1"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunNoArgsPrintsUsageAndFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr should contain usage text, got %q", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Fatalf("stderr should mention the unknown command, got %q", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout should contain usage text, got %q", stdout.String())
	}
}
