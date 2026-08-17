package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sentry/sentry/api/ai/provider"
)

// fakeOllamaServer stands in for a real Ollama server, returning the
// given assistant-message content verbatim -- same reasoning
// queryclient's tests use httptest against a fake api instead of a real
// one: this package's own logic (request shape, response parsing,
// JSON-mode contract) is what's under test, not Ollama itself.
func fakeOllamaServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("unexpected messages shape: %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{Message: chatMessage{Role: "assistant", Content: content}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTranslateParsesJSONResponse(t *testing.T) {
	srv := fakeOllamaServer(t, `{"query": "earliest=-1h severity=ERROR | stats count by service", "confidence": "high", "reason": ""}`)
	c := New(srv.URL, "test-model")

	got, err := c.Translate(context.Background(), provider.TranslateRequest{NLQuery: "errors in the last hour by service"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Confidence != provider.ConfidenceHigh {
		t.Errorf("Confidence = %v, want high", got.Confidence)
	}
	if got.Query == "" {
		t.Error("expected a non-empty query")
	}
}

func TestTranslateHandlesCodeFencedJSON(t *testing.T) {
	srv := fakeOllamaServer(t, "```json\n{\"query\": \"service=api\", \"confidence\": \"medium\", \"reason\": \"\"}\n```")
	c := New(srv.URL, "test-model")

	got, err := c.Translate(context.Background(), provider.TranslateRequest{NLQuery: "api logs"})
	if err != nil {
		t.Fatalf("Translate with fenced JSON: %v", err)
	}
	if got.Query != "service=api" {
		t.Errorf("Query = %q, want %q", got.Query, "service=api")
	}
}

func TestTranslateLowConfidenceCarriesReason(t *testing.T) {
	srv := fakeOllamaServer(t, `{"query": "", "confidence": "low", "reason": "not sure what 'weird stuff' refers to"}`)
	c := New(srv.URL, "test-model")

	got, err := c.Translate(context.Background(), provider.TranslateRequest{NLQuery: "show me weird stuff"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Confidence != provider.ConfidenceLow || got.LowConfidenceReason == "" {
		t.Errorf("got = %+v, want low confidence with a reason", got)
	}
}

func TestTranslateMalformedJSONIsAnError(t *testing.T) {
	srv := fakeOllamaServer(t, "not json at all, sorry")
	c := New(srv.URL, "test-model")

	_, err := c.Translate(context.Background(), provider.TranslateRequest{NLQuery: "anything"})
	if err == nil {
		t.Fatal("expected an error for unparseable model output, got nil")
	}
}

func TestCompleteReturnsSuggestionOnly(t *testing.T) {
	srv := fakeOllamaServer(t, `{"suggestion": " | stats count by host"}`)
	c := New(srv.URL, "test-model")

	got, err := c.Complete(context.Background(), provider.CompleteRequest{QueryPrefix: "service=api", Language: "spl"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Suggestion != " | stats count by host" {
		t.Errorf("Suggestion = %q", got.Suggestion)
	}
}

func TestExplainReturnsPlainText(t *testing.T) {
	srv := fakeOllamaServer(t, "This counts events per host over the last hour.")
	c := New(srv.URL, "test-model")

	got, err := c.Explain(context.Background(), provider.ExplainRequest{Query: "earliest=-1h | stats count by host"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got.Explanation == "" {
		t.Error("expected a non-empty explanation")
	}
}

func TestFixParsesJSONResponse(t *testing.T) {
	srv := fakeOllamaServer(t, `{"suggested_query": "earliest=-1h | stats count", "explanation": "added a required time range", "confidence": "high"}`)
	c := New(srv.URL, "test-model")

	got, err := c.Fix(context.Background(), provider.FixRequest{Query: "stats count", ParseError: "no time range"})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got.SuggestedQuery == "" || got.Explanation == "" {
		t.Errorf("got = %+v, want both fields populated", got)
	}
}

func TestNonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "missing-model")

	_, err := c.Explain(context.Background(), provider.ExplainRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if got := err.Error(); !strings.Contains(got, "model not found") {
		t.Errorf("error = %q, want it to include the server's error message", got)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	c := New("", "m")
	if c.baseURL != "http://localhost:11434" {
		t.Errorf("default baseURL = %q", c.baseURL)
	}
}
