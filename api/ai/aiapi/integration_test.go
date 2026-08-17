// Phase 7 task 13: end-to-end wiring tests distinct from handler_test.go
// and ai/provider/ollama's own tests. Those two files each cover one
// layer in isolation -- handler_test.go's fakeProvider satisfies
// provider.Provider directly, bypassing HTTP/JSON/prompt construction
// entirely; ollama_test.go exercises ollama.Client's wire-format parsing
// against a stub server, but never through aiapi.Handler's actual HTTP
// routes. Neither proves the seam between them actually works: a real
// *ollama.Client wired through *router.Router into a real *Handler,
// driven by real HTTP requests against the registered routes, with real
// planner.Compile/costguard.Assess in the loop.
//
// No live Ollama or model is needed or used -- mockOllamaServer stands
// in for Ollama's real /api/chat wire contract (same technique used for
// this phase's live browser verification, see /docs/phase-7-ai-design.md,
// just returning a fixed canned response instead of one selected by
// inspecting the prompt) with a deterministic canned JSON body, which is
// exactly what makes this suite fast and safe to run in CI -- see the
// "CI testability" section of the design doc for why testing against a
// real model is deliberately kept out of this suite instead.
package aiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentry/sentry/api/ai/provider/ollama"
	"github.com/sentry/sentry/api/ai/router"
)

// jsonBody marshals v for use as an http.Post body -- the integration
// tests below drive real HTTP requests against a real httptest.Server
// (not handler_test.go's doRequest/ResponseRecorder shortcut), since the
// point of this file is proving the routes are actually reachable over
// real HTTP, not just that Handler's methods dispatch correctly.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}
	return bytes.NewReader(b)
}

type fakeInteractionLogger struct {
	entries []InteractionEntry
}

func (f *fakeInteractionLogger) LogInteraction(_ context.Context, entry InteractionEntry) error {
	f.entries = append(f.entries, entry)
	return nil
}

// mockOllamaServer returns an httptest.Server that answers any
// POST /api/chat with the given assistant message content, matching
// Ollama's real response envelope shape byte-for-byte (see
// ollama.go's chatResponse).
func mockOllamaServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected request to %s, want /api/chat", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": content},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newIntegrationHandler(t *testing.T, ollamaContent string) *Handler {
	t.Helper()
	mock := mockOllamaServer(t, ollamaContent)
	client := ollama.New(mock.URL, "test-model")
	r := router.New(client)
	logger := slog.New(slog.NewTextHandler(bytesDiscard{}, nil))
	return NewHandler(logger, r, fakeSchemaSource{}, nil, nil)
}

// TestIntegrationTranslateEndToEnd proves the full path -- HTTP request
// in, real ollama.Client HTTP call out to the mock, real JSON parsing,
// real planner.Compile, real costguard.Assess, HTTP response out --
// works for a query that should pass cleanly (time-bounded, no
// aggregation).
func TestIntegrationTranslateEndToEnd(t *testing.T) {
	h := newIntegrationHandler(t, `{"query":"earliest=-1h severity=ERROR","confidence":"high"}`)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ai/translate", "application/json",
		jsonBody(t, map[string]string{"nlQuery": "errors in the last hour"}))
	if err != nil {
		t.Fatalf("POST /ai/translate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got translateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Query != "earliest=-1h severity=ERROR" {
		t.Errorf("query = %q", got.Query)
	}
	if !got.Compiles {
		t.Error("compiles = false, want true -- this query is valid pipe syntax")
	}
	if got.Blocked {
		t.Errorf("blocked = true, want false: %v", got.CostWarnings)
	}
	if got.Confidence != "high" {
		t.Errorf("confidence = %q, want high", got.Confidence)
	}
}

// TestIntegrationTranslateBlockedByCostGuard proves costguard is
// actually reached through the full HTTP stack, not just unit-tested
// against costguard.Assess in isolation -- an unbounded aggregation
// (stats, no time filter) must come back Blocked.
func TestIntegrationTranslateBlockedByCostGuard(t *testing.T) {
	h := newIntegrationHandler(t, `{"query":"severity=ERROR | stats count by service","confidence":"high"}`)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ai/translate", "application/json",
		jsonBody(t, map[string]string{"nlQuery": "error count by service"}))
	if err != nil {
		t.Fatalf("POST /ai/translate: %v", err)
	}
	defer resp.Body.Close()

	var got translateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !got.Compiles {
		t.Fatalf("compiles = false, want true: %s", got.CompileError)
	}
	if !got.Blocked {
		t.Error("blocked = false, want true -- unbounded aggregation should be rejected by costguard")
	}
	if len(got.CostWarnings) == 0 {
		t.Error("costWarnings is empty, want at least one reason")
	}
}

// TestIntegrationFixEndToEnd proves the same seam for /ai/fix.
func TestIntegrationFixEndToEnd(t *testing.T) {
	h := newIntegrationHandler(t, `{"suggested_query":"earliest=-1h severity=ERROR","explanation":"added a time bound","confidence":"high"}`)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ai/fix", "application/json", jsonBody(t, map[string]string{
		"query":          "severity=ERROR",
		"language":       "spl",
		"executionError": "query timed out: no time range specified",
	}))
	if err != nil {
		t.Fatalf("POST /ai/fix: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got fixResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.SuggestedQuery != "earliest=-1h severity=ERROR" {
		t.Errorf("suggestedQuery = %q", got.SuggestedQuery)
	}
	if got.Blocked {
		t.Errorf("blocked = true, want false: %v", got.CostWarnings)
	}
}

// TestIntegrationCompleteEndToEnd proves the same seam for /ai/complete
// (Track A's ghost-text autocomplete).
func TestIntegrationCompleteEndToEnd(t *testing.T) {
	h := newIntegrationHandler(t, `{"suggestion":" severity=ERROR"}`)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ai/complete", "application/json", jsonBody(t, map[string]string{
		"queryPrefix": "service=api ",
		"language":    "spl",
	}))
	if err != nil {
		t.Fatalf("POST /ai/complete: %v", err)
	}
	defer resp.Body.Close()

	var got completeResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Suggestion != " severity=ERROR" {
		t.Errorf("suggestion = %q", got.Suggestion)
	}
}

// TestIntegrationLogInteractionEndToEnd proves /ai/log-interaction's
// full HTTP decode+validate+dispatch path, using a fake InteractionLogger
// (an in-process Go fake is the right seam here, not another mock HTTP
// server -- the real implementation is enterprise/internal/audit, which
// needs a live Postgres and is covered by that package's own tests).
func TestIntegrationLogInteractionEndToEnd(t *testing.T) {
	logger := &fakeInteractionLogger{}
	r := router.New(&fakeProvider{})
	h := NewHandler(slog.New(slog.NewTextHandler(bytesDiscard{}, nil)), r, fakeSchemaSource{}, nil, logger)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ai/log-interaction", "application/json", jsonBody(t, map[string]any{
		"operation":  "translate",
		"input":      "errors in the last hour",
		"output":     "earliest=-1h severity=ERROR",
		"confidence": "high",
		"accepted":   true,
		"edited":     false,
		"finalQuery": "earliest=-1h severity=ERROR",
	}))
	if err != nil {
		t.Fatalf("POST /ai/log-interaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(logger.entries) != 1 {
		t.Fatalf("got %d logged entries, want 1", len(logger.entries))
	}
	if logger.entries[0].Operation != "translate" || !logger.entries[0].Accepted {
		t.Errorf("logged entry = %+v", logger.entries[0])
	}
}
