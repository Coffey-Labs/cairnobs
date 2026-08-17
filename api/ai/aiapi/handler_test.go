package aiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentry/sentry/api/ai/provider"
	"github.com/sentry/sentry/api/ai/router"
)

type fakeProvider struct {
	translateResult provider.TranslateResult
	completeResult  provider.CompleteResult
	explainResult   provider.ExplainResult
	fixResult       provider.FixResult
	err             error

	gotExplainReq provider.ExplainRequest
}

func (f *fakeProvider) Translate(context.Context, provider.TranslateRequest) (provider.TranslateResult, error) {
	return f.translateResult, f.err
}
func (f *fakeProvider) Complete(context.Context, provider.CompleteRequest) (provider.CompleteResult, error) {
	return f.completeResult, f.err
}
func (f *fakeProvider) Explain(_ context.Context, req provider.ExplainRequest) (provider.ExplainResult, error) {
	f.gotExplainReq = req
	return f.explainResult, f.err
}
func (f *fakeProvider) Fix(context.Context, provider.FixRequest) (provider.FixResult, error) {
	return f.fixResult, f.err
}

type fakeSchemaSource struct{}

func (fakeSchemaSource) SchemaContext(context.Context) provider.SchemaContext {
	return provider.SchemaContext{Services: []string{"api"}}
}

func newTestHandler(p *fakeProvider) *Handler {
	r := router.New(p)
	logger := slog.New(slog.NewTextHandler(bytesDiscard{}, nil))
	return NewHandler(logger, r, fakeSchemaSource{}, nil, nil)
}

type bytesDiscard struct{}

func (bytesDiscard) Write(p []byte) (int, error) { return len(p), nil }

func doRequest(t *testing.T, h *Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleCompleteReturnsSuggestion(t *testing.T) {
	p := &fakeProvider{completeResult: provider.CompleteResult{Suggestion: " | stats count"}}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/complete", completeRequest{QueryPrefix: "service=api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp completeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Suggestion != " | stats count" {
		t.Errorf("Suggestion = %q", resp.Suggestion)
	}
}

func TestHandleCompleteDegradesGracefullyOnProviderError(t *testing.T) {
	p := &fakeProvider{err: errors.New("provider down")}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/complete", completeRequest{QueryPrefix: "service=api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on provider failure (graceful degradation), body = %s", rec.Code, rec.Body.String())
	}
	var resp completeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Suggestion != "" {
		t.Errorf("Suggestion = %q, want empty on provider failure", resp.Suggestion)
	}
}

func TestHandleCompleteEmptyPrefixSkipsProviderCall(t *testing.T) {
	p := &fakeProvider{err: errors.New("should not be called")}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/complete", completeRequest{QueryPrefix: "   "})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleExplainReturnsExplanation(t *testing.T) {
	p := &fakeProvider{explainResult: provider.ExplainResult{Explanation: "counts errors per host"}}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/explain", explainRequest{Query: "severity=ERROR | stats count by host"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp explainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Explanation == "" {
		t.Error("expected a non-empty explanation")
	}
}

func TestHandleExplainEmptyQueryIsBadRequest(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	rec := doRequest(t, h, "POST", "/ai/explain", explainRequest{Query: ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleExplainProviderErrorIsBadGateway(t *testing.T) {
	p := &fakeProvider{err: errors.New("model unavailable")}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/explain", explainRequest{Query: "service=api"})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleFixMissingErrorFieldsIsBadRequest(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	rec := doRequest(t, h, "POST", "/ai/fix", fixRequest{Query: "service=api"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (neither parseError nor executionError set)", rec.Code)
	}
}

func TestHandleFixAssessesSuggestedQueryCost(t *testing.T) {
	// The provider suggests a fix that aggregates with no time bound --
	// costguard should reject it (an aggregation gets no implicit row
	// cap the way a raw-row fetch does), and the handler must mark it
	// Blocked rather than silently offering it as runnable. Needs a real
	// leading pipe stage -- bare words with no "|" parse as free-text
	// search terms, not an aggregation, per the query grammar.
	p := &fakeProvider{fixResult: provider.FixResult{
		SuggestedQuery: "service=api | stats count by host",
		Explanation:    "removed the invalid field reference",
		Confidence:     provider.ConfidenceHigh,
	}}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/fix", fixRequest{Query: "bogus_field=1 | stats count by host", ParseError: "unknown field"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp fixResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Blocked {
		t.Errorf("resp = %+v, want Blocked=true for an unbounded aggregation suggestion", resp)
	}
	if len(resp.CostWarnings) == 0 {
		t.Error("expected non-empty CostWarnings")
	}
}

func TestHandleFixBoundedSuggestionIsNotBlocked(t *testing.T) {
	p := &fakeProvider{fixResult: provider.FixResult{
		SuggestedQuery: "earliest=-1h | stats count by host",
		Confidence:     provider.ConfidenceHigh,
	}}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/fix", fixRequest{Query: "stats count by host", ParseError: "no time range"})
	var resp fixResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Blocked {
		t.Errorf("resp = %+v, want Blocked=false for a properly time-bounded suggestion", resp)
	}
}

func TestHandleOptimizeNoFindingsForBoundedQuery(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	rec := doRequest(t, h, "POST", "/ai/optimize", optimizeRequest{Query: "earliest=-1h severity=ERROR | stats count by host"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp optimizeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Findings) != 0 || resp.Phrased != "" {
		t.Errorf("resp = %+v, want no findings for a bounded query", resp)
	}
}

func TestHandleOptimizeSuggestsMechanicalFixForMissingTimeRange(t *testing.T) {
	p := &fakeProvider{explainResult: provider.ExplainResult{Explanation: "add a time range to avoid scanning everything"}}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/optimize", optimizeRequest{Query: "stats count by host"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp optimizeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Findings) == 0 {
		t.Error("expected at least one finding for an unbounded aggregation")
	}
	if resp.SuggestedQuery != "earliest=-1h stats count by host" {
		t.Errorf("SuggestedQuery = %q", resp.SuggestedQuery)
	}
	if resp.Phrased == "" {
		t.Error("expected a phrased explanation from the (fake) provider")
	}
	if len(p.gotExplainReq.RuleFindings) == 0 {
		t.Error("expected Explain to have been called with RuleFindings set")
	}
}

func TestHandleOptimizeDegradesGracefullyWhenPhraseFails(t *testing.T) {
	p := &fakeProvider{err: errors.New("model down")}
	h := newTestHandler(p)

	rec := doRequest(t, h, "POST", "/ai/optimize", optimizeRequest{Query: "stats count by host"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when phrasing fails, body = %s", rec.Code, rec.Body.String())
	}
	var resp optimizeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Findings) == 0 {
		t.Error("Findings should still be populated (rule-based, no model needed) even if phrasing fails")
	}
	if resp.Phrased != "" {
		t.Errorf("Phrased = %q, want empty when the provider fails", resp.Phrased)
	}
}

func TestHandleOptimizeInvalidQueryIsBadRequest(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	rec := doRequest(t, h, "POST", "/ai/optimize", optimizeRequest{Query: "| stats"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an uncompilable query", rec.Code)
	}
}

// ---- translate ----

func TestHandleTranslateEmptyNLQueryIsBadRequest(t *testing.T) {
	h := newTestHandler(&fakeProvider{})
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleTranslateProviderErrorIsBadGateway(t *testing.T) {
	p := &fakeProvider{err: errors.New("model unavailable")}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "errors in the last hour"})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestHandleTranslateBoundedQueryCompilesCleanly(t *testing.T) {
	p := &fakeProvider{translateResult: provider.TranslateResult{
		Query:      "earliest=-1h severity=ERROR | stats count by service",
		Confidence: provider.ConfidenceHigh,
	}}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "errors per service in the last hour"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp translateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Compiles || resp.CompileError != "" {
		t.Errorf("resp = %+v, want Compiles=true, no CompileError", resp)
	}
	if resp.Blocked || len(resp.CostWarnings) != 0 {
		t.Errorf("resp = %+v, want no cost warnings for a time-bounded query", resp)
	}
	if resp.Confidence != string(provider.ConfidenceHigh) {
		t.Errorf("Confidence = %q", resp.Confidence)
	}
}

func TestHandleTranslateUnboundedQueryIsBlocked(t *testing.T) {
	// The model produced a syntactically valid but unbounded aggregation
	// -- task 9's explicit requirement that translation results run
	// through the same cost guard AI-suggested fixes do.
	p := &fakeProvider{translateResult: provider.TranslateResult{
		Query:      "severity=ERROR | stats count by service",
		Confidence: provider.ConfidenceHigh,
	}}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "errors by service"})
	var resp translateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Compiles {
		t.Fatalf("resp = %+v, want Compiles=true", resp)
	}
	if !resp.Blocked || len(resp.CostWarnings) == 0 {
		t.Errorf("resp = %+v, want Blocked=true with cost warnings for an unbounded aggregation", resp)
	}
}

func TestHandleTranslateNonCompilingQueryIsHonestlyReported(t *testing.T) {
	// The model returned something that doesn't actually parse -- a
	// real, distinct outcome from low confidence (a confident model can
	// still produce invalid syntax); the handler must say so plainly,
	// not silently drop it or crash.
	p := &fakeProvider{translateResult: provider.TranslateResult{
		Query:      "| stats count",
		Confidence: provider.ConfidenceHigh,
	}}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "something odd"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a non-compiling suggestion is a reportable outcome, not an HTTP error), body = %s", rec.Code, rec.Body.String())
	}
	var resp translateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Compiles || resp.CompileError == "" {
		t.Errorf("resp = %+v, want Compiles=false with a CompileError", resp)
	}
}

func TestHandleTranslateLowConfidenceCarriesReason(t *testing.T) {
	p := &fakeProvider{translateResult: provider.TranslateResult{
		Confidence:          provider.ConfidenceLow,
		LowConfidenceReason: "not sure what 'weird stuff' refers to",
	}}
	h := newTestHandler(p)
	rec := doRequest(t, h, "POST", "/ai/translate", translateRequest{NLQuery: "show me weird stuff"})
	var resp translateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Query != "" {
		t.Errorf("Query = %q, want empty for a low-confidence non-answer", resp.Query)
	}
	if resp.Confidence != string(provider.ConfidenceLow) || resp.LowConfidenceReason == "" {
		t.Errorf("resp = %+v, want low confidence with a reason", resp)
	}
	if resp.Compiles {
		t.Errorf("resp = %+v, want Compiles=false when Query is empty", resp)
	}
}
