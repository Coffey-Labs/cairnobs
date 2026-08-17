// Package provider defines the model-provider abstraction every AI-assisted
// query feature (Phase 7) is built on: translate, complete, explain, fix.
// Same narrow-interface pattern as querylang/executor's SQLRunner/
// SearchClient -- a small interface a production implementation
// (provider/ollama, the default; a cloud adapter, opt-in) and a fake
// (for tests) both satisfy, so nothing above this layer needs to know or
// care which model actually answered.
//
// What this package deliberately does NOT do: decide *which* provider or
// model answers a given request. That's a routing concern (per-operation
// config, per-tenant cloud opt-in) that lives one layer up, once task 3/4
// land -- this package only defines the shape every provider must speak.
//
// Every operation is grounded (SchemaContext) and every result that
// produces a query is designed to flow through the unchanged Phase 2
// planner.Compile -> executor.Execute path before it ever runs -- this
// package returns query *text*, never executes anything itself. See
// /docs/phase-7-ai-design.md for the full design this interface was
// built against.
package provider

import "context"

// SchemaContext is the grounding data every operation receives -- known
// service names, field names (structured columns plus common attribute
// keys), and value examples for enum-like fields (severity, status, and
// so on). Sourced from ClickHouse system tables / periodic sampling
// (task 3), never hand-maintained, and always scoped to the requesting
// tenant's own data -- a provider implementation must never be handed
// another tenant's grounding data, the same connection-layer-isolation
// discipline Phase 4 applies to query execution itself. This package
// doesn't resolve SchemaContext; callers (the schema/metadata service,
// task 3) build it and pass it in, so a Provider implementation never
// needs ClickHouse access of its own.
type SchemaContext struct {
	Services []string
	// Fields covers both real columns (timestamp, host, service,
	// severity, message, record_id) and the common attribute keys seen
	// in the tenant's own data -- see /docs/query-language-reference.md's
	// "Field mapping" section for why the distinction mostly doesn't
	// matter to a query author, and shouldn't need to matter to the model
	// either.
	Fields []FieldInfo
}

type FieldInfo struct {
	Name string
	// Examples is a short, representative sample of real values seen for
	// this field -- most useful for enum-like fields (severity, status)
	// where showing the model the actual vocabulary beats describing it.
	// Empty for high-cardinality fields (host, message) where examples
	// wouldn't help and would just spend context budget.
	Examples []string
}

// Confidence is deliberately a small enum, not a raw float -- a model's
// self-reported numeric confidence isn't a calibrated probability, and
// pretending it is (via e.g. "reject anything under 0.73") invites false
// precision. Three bands are enough to drive real UI behavior (task 10's
// "handle low-confidence translation honestly") without pretending to
// more precision than a model's self-assessment actually has.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

type TranslateRequest struct {
	NLQuery string
	Schema  SchemaContext
}

type TranslateResult struct {
	// Query is Phase 2 pipe-syntax, never raw SQL -- the phase brief's
	// explicit "narrower, safer surface" choice for generation targets.
	// A provider that can't produce a valid completion should return an
	// error, not a best-effort raw-SQL fallback.
	Query      string
	Confidence Confidence
	// LowConfidenceReason is set (and Query may be empty) when the
	// provider can't produce a translation it's willing to stand behind
	// at all -- task 10 wants this said plainly, not papered over with a
	// guess. Empty when Confidence is High or Medium.
	LowConfidenceReason string
}

type CompleteRequest struct {
	// QueryPrefix is everything the user has typed so far, cursor at the
	// end -- this operation is a full-completion suggestion (ghost text),
	// not a fill-in-the-middle edit, matching how the query bar's cursor
	// behaves (Phase 5's QueryEditor.svelte, always append-at-cursor).
	QueryPrefix string
	Language    string // "spl" or "sql", never "" -- the caller has always already resolved auto-detection by this point
	Schema      SchemaContext
}

type CompleteResult struct {
	// Suggestion is the suggested continuation only (what ghost-text
	// should render after the cursor), not QueryPrefix+continuation
	// restated -- keeps the caller from having to diff its own input
	// back out of the result.
	Suggestion string
	// Empty Suggestion (with no error) is a legitimate response -- "no
	// good completion here" is not the same failure mode as a timeout or
	// a down provider, and the caller (task 5's fallback logic) needs to
	// tell them apart.
}

type ExplainRequest struct {
	Query    string
	Language string
	// OriginalIntent, when non-empty, means this Explain call is
	// reviewing a just-translated query (Track B) rather than an
	// arbitrary hand-written one (Track A) -- same operation, task 10's
	// explicit "reuse explain rather than build a separate mechanism"
	// choice, but the prompt can speak to *how the NL became this query*
	// instead of only describing the query in isolation.
	OriginalIntent string
	// RuleFindings, when non-empty, means this Explain call is task 8's
	// Optimize suggestion: rule-based detection (costguard) already found
	// something worth flagging, and the model's only job is phrasing
	// those specific findings clearly for a user -- not describing what
	// the query does, not detecting the inefficiency itself. Mutually
	// exclusive with OriginalIntent in practice (a query is either being
	// explained, reviewed post-translation, or optimized), but the type
	// doesn't need to enforce that -- three prompt-shaping contexts for
	// one operation, matching the same reuse-over-duplication choice
	// OriginalIntent already made rather than adding a fifth Provider
	// method for what is still, underneath, "explain something about
	// this query in plain English."
	RuleFindings []string
}

type ExplainResult struct {
	Explanation string
}

type FixRequest struct {
	Query    string
	Language string
	// ParseError is set when the query never compiled at all (planner
	// error text); ExecutionError is set when it compiled but failed at
	// runtime (executor/ClickHouse error text). Exactly one is set --
	// the two failure modes want different framing ("this doesn't parse
	// because..." vs. "this ran but...").
	ParseError     string
	ExecutionError string
	Schema         SchemaContext
}

type FixResult struct {
	// SuggestedQuery is the full corrected query text, always shown as a
	// diff against the original by the caller (task 7's explicit
	// "never silently applied" requirement) -- this package only
	// produces the suggestion, the UI owns the diff rendering and the
	// accept/dismiss decision.
	SuggestedQuery string
	// Explanation is a short plain-English note on what was wrong and
	// what changed -- distinct from Explain's job (describing what a
	// query *does*), this describes what was *fixed* and why.
	Explanation string
	Confidence  Confidence
}

// Provider is what every model backend implements: the default
// self-hosted Ollama provider, the opt-in cloud adapter, and a fake for
// tests. Every method takes a context so a caller can enforce the tight
// latency budget Complete needs (task 5) without the interface itself
// hard-coding a timeout -- that's a caller concern, since the right
// timeout differs by operation (Complete's is much tighter than
// Translate's).
type Provider interface {
	Translate(ctx context.Context, req TranslateRequest) (TranslateResult, error)
	Complete(ctx context.Context, req CompleteRequest) (CompleteResult, error)
	Explain(ctx context.Context, req ExplainRequest) (ExplainResult, error)
	Fix(ctx context.Context, req FixRequest) (FixResult, error)
}
