// Package ollama implements provider.Provider against a local Ollama
// server -- the default, self-hosted primary provider (Phase 7 task 2;
// see /docs/phase-7-ai-design.md for why Ollama over vLLM and why
// qwen2.5-coder is the recommended model). Same thin-HTTP-client shape
// as alerting/internal/queryclient -- net/http + encoding/json, no new
// HTTP client dependency, matching this codebase's "boring,
// well-understood dependencies" convention.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cairnobs/cairnobs/api/ai/provider"
)

// Client implements provider.Provider against one Ollama server and one
// model. The per-operation routing layer (task 2's "per-operation
// provider/model configuration") constructs one Client per distinct
// model a deployment configures -- e.g. one for qwen2.5-coder:1.5b
// (Complete's fast path) and one for qwen2.5-coder:7b (everything else)
// -- rather than this package knowing anything about operation-to-model
// routing itself.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// New builds a Client. baseURL defaults to Ollama's standard local
// address if empty -- the common case for the primary, self-hosted
// deployment target.
func New(baseURL, model string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), model: model, http: &http.Client{}}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Format   string        `json:"format,omitempty"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
}

// chat calls Ollama's POST /api/chat, non-streaming, and returns the
// assistant message content. jsonMode requests Ollama's JSON-constrained
// output format -- used by every operation except Explain, which just
// wants prose back.
func (c *Client) chat(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	req := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream: false,
	}
	if jsonMode {
		req.Format = "json"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("ollama: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama: calling %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			return "", fmt.Errorf("ollama: request failed (%d): %s", resp.StatusCode, errBody.Error)
		}
		return "", fmt.Errorf("ollama: request failed with status %d", resp.StatusCode)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("ollama: decoding response: %w", err)
	}
	return chatResp.Message.Content, nil
}

// stripCodeFence handles the common small-model habit of wrapping JSON
// output in ```json ... ``` even when explicitly told not to -- a
// best-effort cleanup, not a guarantee; a model that returns genuinely
// malformed JSON still surfaces as a real decode error to the caller,
// which is the correct behavior (better an explicit error than silently
// fabricating a result).
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func parseConfidence(s string) provider.Confidence {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return provider.ConfidenceHigh
	case "medium":
		return provider.ConfidenceMedium
	default:
		// Unrecognized or missing confidence fails toward caution, not
		// toward assumed correctness -- an empty/garbled confidence
		// field from the model is itself a signal something's off.
		return provider.ConfidenceLow
	}
}

func (c *Client) Translate(ctx context.Context, req provider.TranslateRequest) (provider.TranslateResult, error) {
	raw, err := c.chat(ctx, translateSystemPrompt(req.Schema), req.NLQuery, true)
	if err != nil {
		return provider.TranslateResult{}, err
	}
	var parsed struct {
		Query      string `json:"query"`
		Confidence string `json:"confidence"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(raw)), &parsed); err != nil {
		return provider.TranslateResult{}, fmt.Errorf("ollama: parsing translate response: %w", err)
	}
	return provider.TranslateResult{
		Query:               parsed.Query,
		Confidence:          parseConfidence(parsed.Confidence),
		LowConfidenceReason: parsed.Reason,
	}, nil
}

func (c *Client) Complete(ctx context.Context, req provider.CompleteRequest) (provider.CompleteResult, error) {
	raw, err := c.chat(ctx, completeSystemPrompt(req.Schema), req.QueryPrefix, true)
	if err != nil {
		return provider.CompleteResult{}, err
	}
	var parsed struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(raw)), &parsed); err != nil {
		return provider.CompleteResult{}, fmt.Errorf("ollama: parsing complete response: %w", err)
	}
	return provider.CompleteResult{Suggestion: parsed.Suggestion}, nil
}

func (c *Client) Explain(ctx context.Context, req provider.ExplainRequest) (provider.ExplainResult, error) {
	user := req.Query
	switch {
	case len(req.RuleFindings) > 0:
		user = fmt.Sprintf("Query: %s\nFindings: %s", req.Query, strings.Join(req.RuleFindings, "; "))
	case req.OriginalIntent != "":
		user = fmt.Sprintf("Original request: %q\nGenerated query: %s", req.OriginalIntent, req.Query)
	}
	raw, err := c.chat(ctx, explainSystemPrompt(req.OriginalIntent != "", len(req.RuleFindings) > 0), user, false)
	if err != nil {
		return provider.ExplainResult{}, err
	}
	return provider.ExplainResult{Explanation: strings.TrimSpace(raw)}, nil
}

func (c *Client) Fix(ctx context.Context, req provider.FixRequest) (provider.FixResult, error) {
	errText := req.ParseError
	if errText == "" {
		errText = req.ExecutionError
	}
	user := fmt.Sprintf("Query: %s\nError: %s", req.Query, errText)
	raw, err := c.chat(ctx, fixSystemPrompt(req.Schema), user, true)
	if err != nil {
		return provider.FixResult{}, err
	}
	var parsed struct {
		SuggestedQuery string `json:"suggested_query"`
		Explanation    string `json:"explanation"`
		Confidence     string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(raw)), &parsed); err != nil {
		return provider.FixResult{}, fmt.Errorf("ollama: parsing fix response: %w", err)
	}
	return provider.FixResult{
		SuggestedQuery: parsed.SuggestedQuery,
		Explanation:    parsed.Explanation,
		Confidence:     parseConfidence(parsed.Confidence),
	}, nil
}

var _ provider.Provider = (*Client)(nil)
