package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdAgentsMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAgentsListSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agents" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"host":"web-01","service":"web"}]`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"list", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "web-01") {
		t.Fatalf("stdout = %q, want it to contain the listed agent", stdout.String())
	}
}

func TestCmdAgentsGetMissingHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"get"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAgentsGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/web-01" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"web-01"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"get", "web-01", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}

func TestCmdAgentsConfigMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAgentsConfigClearSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/agents/web-01/config" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "clear", "web-01", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cleared") {
		t.Fatalf("stdout = %q, want a confirmation", stdout.String())
	}
}

func TestCmdAgentsConfigSetRequiresAtLeastOneFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "set", "web-01"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "at least one of") {
		t.Fatalf("stderr = %q, want it to explain a flag is required", stderr.String())
	}
}

func TestCmdAgentsConfigSetInvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "set", "web-01", "--batch-max-size", "not-a-number"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

// TestCmdAgentsConfigSetMergesUntouchedFields is the regression test for
// the whole point of the merge logic: setting only --heartbeat-interval-ms
// must carry forward the agent's OTHER already-set override field
// (batch_max_size) and its reported (non-overridden) values for
// everything else, not silently reset them.
func TestCmdAgentsConfigSetMergesUntouchedFields(t *testing.T) {
	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agents/web-01":
			w.Write([]byte(`{
				"source_kind": "journald",
				"batch_max_size": 500,
				"batch_flush_interval_ms": 2000,
				"heartbeat_enabled": true,
				"heartbeat_interval_ms": 60000,
				"desired_override": {"batch_max_size": 1000}
			}`))
		case r.Method == http.MethodPut && r.URL.Path == "/agents/web-01/config":
			putBody, _ = io.ReadAll(r.Body)
			w.Write(putBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "set", "web-01", "--heartbeat-interval-ms", "30000", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}

	var sent agentConfigOverride
	if err := json.Unmarshal(putBody, &sent); err != nil {
		t.Fatalf("decoding PUT body: %v", err)
	}
	if sent.BatchMaxSize == nil || *sent.BatchMaxSize != 1000 {
		t.Fatalf("BatchMaxSize = %v, want 1000 (carried forward from the existing override)", sent.BatchMaxSize)
	}
	if sent.BatchFlushIntervalMS == nil || *sent.BatchFlushIntervalMS != 2000 {
		t.Fatalf("BatchFlushIntervalMS = %v, want 2000 (carried forward from reported value)", sent.BatchFlushIntervalMS)
	}
	if sent.HeartbeatEnabled == nil || *sent.HeartbeatEnabled != true {
		t.Fatalf("HeartbeatEnabled = %v, want true (carried forward from reported value)", sent.HeartbeatEnabled)
	}
	if sent.HeartbeatIntervalMS == nil || *sent.HeartbeatIntervalMS != 30000 {
		t.Fatalf("HeartbeatIntervalMS = %v, want 30000 (the flag actually passed)", sent.HeartbeatIntervalMS)
	}
}

// TestCmdAgentsConfigSetOmitsJournaldUnitForNonJournaldSource mirrors
// web/src/routes/agents/[host]/+page.svelte's save(): journald_unit
// must never be sent for an agent whose source isn't journald, even if
// a stale override somehow had one.
func TestCmdAgentsConfigSetOmitsJournaldUnitForNonJournaldSource(t *testing.T) {
	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"source_kind":"file","batch_max_size":500,"batch_flush_interval_ms":2000,"heartbeat_enabled":true,"heartbeat_interval_ms":60000}`))
		case r.Method == http.MethodPut:
			putBody, _ = io.ReadAll(r.Body)
			w.Write(putBody)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "set", "file-host", "--batch-max-size", "100", "--api", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(string(putBody), "journald_unit") {
		t.Fatalf("PUT body = %s, must not carry journald_unit for a non-journald source", putBody)
	}
}

func TestCmdAgentsConfigSetFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agent not found"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"config", "set", "nope", "--batch-max-size", "100", "--api", srv.URL}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "agent not found") {
		t.Fatalf("stderr = %q, want the server's actual error surfaced", stderr.String())
	}
}

func TestCmdAgentsRestartMissingHost(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"restart"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAgentsRestartWithYesSkipsConfirmation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/agents/web-01/command" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"command":"restart"`) {
			t.Errorf("body = %s, want command=restart", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"web-01","pending_command":"restart"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := cmdAgentsRestart([]string{"web-01", "--yes"}, srv.URL, "", strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
}

// The interactive confirm-prompt branch (confirmRun asked, "y"/"n"
// answered) isn't reachable in a test the way cmdAgentsRestart is
// structured -- isInteractive checks the concrete *os.File type, which
// a strings.Reader can never satisfy, same constraint cmd_query.go's
// own tests work around by testing confirmRun directly (see
// TestConfirmRunAcceptsY/TestConfirmRunRejectsBlankAndOther in
// cmd_query_test.go) rather than through the full non-interactive gate.
// Those two generic tests already cover the y/n logic this command
// relies on; only the two paths actually reachable with a non-tty
// stdin -- --yes and no-confirmation-possible -- are tested below.

// TestCmdAgentsRestartNonInteractiveWithoutYesRefuses guards against a
// scripted/piped invocation hanging forever waiting for an answer
// nobody can give -- same posture as cmd_query.go's isInteractive check
// for --nl without --execute.
func TestCmdAgentsRestartNonInteractiveWithoutYesRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not call the server without --yes when stdin isn't a terminal")
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	// strings.Reader is never a *os.File, so isInteractive(it) is
	// always false -- exercising the same "piped stdin" path a real
	// non-interactive invocation would hit.
	code := cmdAgentsRestart([]string{"web-01"}, srv.URL, "", strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "--yes") {
		t.Fatalf("stdout = %q, want it to mention --yes", stdout.String())
	}
}

func TestCmdAgentsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAgents([]string{"bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}
