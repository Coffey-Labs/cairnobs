package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseQueryArgsNL(t *testing.T) {
	qa := parseQueryArgs([]string{"--nl", "errors in the last hour", "--execute"}, func(string) string { return "" })
	if qa.nlQuery != "errors in the last hour" {
		t.Errorf("nlQuery = %q", qa.nlQuery)
	}
	if !qa.execute {
		t.Error("execute = false, want true")
	}
}

func TestCmdQueryNLLowConfidenceDoesNotRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/translate" {
			t.Errorf("unexpected request to %s, want only /ai/translate (never /query)", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"","confidence":"low","lowConfidenceReason":"not sure what that means"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--nl", "show me weird stuff", "--execute", "--api", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1 (no confident translation)", code)
	}
	if !strings.Contains(stdout.String(), "not sure what that means") {
		t.Errorf("stdout = %q, want the low-confidence reason", stdout.String())
	}
}

func TestCmdQueryNLBlockedIsNotRunEvenWithExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			t.Error("a blocked translation must never reach /query, even with --execute")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"severity=ERROR | stats count by service","confidence":"high","compiles":true,"blocked":true,"costWarnings":["no time range filter, and this query aggregates"]}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--nl", "errors by service", "--execute", "--api", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1 (blocked)", code)
	}
	if !strings.Contains(stdout.String(), "Not offered as directly runnable") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestCmdQueryNLNonCompilingDoesNotRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			t.Error("a non-compiling translation must never reach /query")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"| stats count","confidence":"high","compiles":false,"compileError":"expected a filter, comparison, or search term, got PIPE"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--nl", "something odd", "--execute", "--api", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "does not parse") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestCmdQueryNLWithExecuteRunsTheQuery(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ai/translate":
			w.Write([]byte(`{"query":"earliest=-1h severity=ERROR | stats count by service","confidence":"high","compiles":true,"blocked":false}`))
		case "/query":
			sawQuery = "called"
			w.Write([]byte(`{"columns":["service","count"],"rows":[["api",5]]}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--nl", "errors per service in the last hour", "--execute", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if sawQuery != "called" {
		t.Error("expected /query to be called with --execute set")
	}
	if !strings.Contains(stdout.String(), "api") {
		t.Errorf("stdout = %q, want the query results printed", stdout.String())
	}
}

func TestCmdQueryNLWithoutExecuteNonInteractiveDoesNotRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			t.Error("must not run without --execute when stdin isn't a terminal")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"query":"earliest=-1h | stats count","confidence":"high","compiles":true,"blocked":false}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--nl", "how many events", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("code = %d, want 0 (declining to run isn't a failure)", code)
	}
	if !strings.Contains(stdout.String(), "Not running") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfirmRunAcceptsY(t *testing.T) {
	var stdout bytes.Buffer
	if !confirmRun(strings.NewReader("y\n"), &stdout, "Run?") {
		t.Error("expected 'y' to confirm")
	}
}

func TestConfirmRunRejectsBlankAndOther(t *testing.T) {
	var stdout bytes.Buffer
	if confirmRun(strings.NewReader("\n"), &stdout, "Run?") {
		t.Error("expected a blank line to NOT confirm")
	}
	if confirmRun(strings.NewReader("sure\n"), &stdout, "Run?") {
		t.Error("expected an unrecognized answer to NOT confirm")
	}
}
