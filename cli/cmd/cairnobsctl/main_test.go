package main

import (
	"bytes"
	"encoding/json"
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
		if k == "CAIRNOBSCTL_API_URL" {
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
		if k == "CAIRNOBSCTL_API_URL" {
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

func TestParseQueryArgsJoinsNonFlagArgsAsQuery(t *testing.T) {
	env := func(string) string { return "" }
	qa := parseQueryArgs([]string{"service=api", "status=500"}, env)
	if qa.query != "service=api status=500" {
		t.Errorf("query = %q", qa.query)
	}
	if qa.apiURL != defaultAPIURL {
		t.Errorf("apiURL = %q, want default", qa.apiURL)
	}
	if qa.jsonOut {
		t.Error("jsonOut should default to false")
	}
}

func TestParseQueryArgsFlags(t *testing.T) {
	env := func(string) string { return "" }
	qa := parseQueryArgs([]string{"--api", "http://h:1", "--language", "sql", "--json", "SELECT", "1"}, env)
	if qa.apiURL != "http://h:1" {
		t.Errorf("apiURL = %q", qa.apiURL)
	}
	if qa.language != "sql" {
		t.Errorf("language = %q", qa.language)
	}
	if !qa.jsonOut {
		t.Error("expected jsonOut = true")
	}
	if qa.query != "SELECT 1" {
		t.Errorf("query = %q", qa.query)
	}
}

func TestCmdQueryMissingQueryErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdQuery(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing query") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCmdQueryTableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body queryRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if body.Query != "service=api" {
			t.Errorf("query = %q", body.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queryResponseBody{
			Columns: []string{"host", "count"},
			Rows:    [][]any{{"h1", float64(3)}},
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--api", srv.URL, "service=api"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "host") || !strings.Contains(out, "h1") || !strings.Contains(out, "(1 row(s))") {
		t.Fatalf("unexpected table output: %q", out)
	}
}

func TestCmdQueryJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queryResponseBody{Columns: []string{"c"}, Rows: [][]any{{"v"}}})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--api", srv.URL, "--json", "service=api"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var got queryResponseBody
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v; got %q", err, stdout.String())
	}
	if len(got.Columns) != 1 || got.Columns[0] != "c" {
		t.Fatalf("unexpected JSON output: %+v", got)
	}
}

func TestCmdQueryServerErrorPrintsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(errorResponseBody{Error: "only SELECT queries are allowed"})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdQuery([]string{"--api", srv.URL, "DELETE FROM logs", "--language", "sql"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only SELECT queries are allowed") {
		t.Fatalf("stderr = %q, want it to include the server's error message", stderr.String())
	}
}

func TestFormatCellHandlesNilMapAndSlice(t *testing.T) {
	if got := formatCell(nil); got != "" {
		t.Errorf("formatCell(nil) = %q, want empty", got)
	}
	if got := formatCell(map[string]any{"a": "b"}); got != `{"a":"b"}` {
		t.Errorf("formatCell(map) = %q", got)
	}
	if got := formatCell(42.0); got != "42" {
		t.Errorf("formatCell(42.0) = %q", got)
	}
}
