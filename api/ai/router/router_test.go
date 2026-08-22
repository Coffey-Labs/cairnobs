package router

import (
	"context"
	"testing"

	"github.com/cairnobs/cairnobs/api/ai/provider"
)

// namedFakeProvider lets a test tell which configured provider actually
// answered a call -- the thing router.For needs to get right.
type namedFakeProvider struct {
	name string
}

func (f *namedFakeProvider) Translate(context.Context, provider.TranslateRequest) (provider.TranslateResult, error) {
	return provider.TranslateResult{Query: f.name}, nil
}
func (f *namedFakeProvider) Complete(context.Context, provider.CompleteRequest) (provider.CompleteResult, error) {
	return provider.CompleteResult{Suggestion: f.name}, nil
}
func (f *namedFakeProvider) Explain(context.Context, provider.ExplainRequest) (provider.ExplainResult, error) {
	return provider.ExplainResult{Explanation: f.name}, nil
}
func (f *namedFakeProvider) Fix(context.Context, provider.FixRequest) (provider.FixResult, error) {
	return provider.FixResult{SuggestedQuery: f.name}, nil
}

var _ provider.Provider = (*namedFakeProvider)(nil)

func TestForReturnsFallbackWhenUnconfigured(t *testing.T) {
	fallback := &namedFakeProvider{name: "default"}
	r := New(fallback)

	if got := r.For(OpTranslate); got != fallback {
		t.Errorf("For(OpTranslate) = %v, want the fallback provider", got)
	}
}

func TestSetOperationOverridesFallback(t *testing.T) {
	fallback := &namedFakeProvider{name: "default"}
	fast := &namedFakeProvider{name: "fast"}
	r := New(fallback)
	r.SetOperation(OpComplete, fast)

	if got := r.For(OpComplete); got != fast {
		t.Errorf("For(OpComplete) = %v, want the fast override", got)
	}
	if got := r.For(OpTranslate); got != fallback {
		t.Errorf("For(OpTranslate) = %v, want unaffected fallback", got)
	}
}
