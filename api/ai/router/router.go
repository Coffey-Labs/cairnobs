// Package router is the per-operation provider/model dispatch layer
// /docs/phase-7-ai-design.md's "per-operation provider/model
// configuration" section decided to build now rather than defer --
// Complete's tight latency budget and Translate/Fix's quality needs are
// already in tension under a single-model-for-everything design, not a
// hypothetical future conflict.
//
// Deliberately thin: a lookup table from Operation to whichever
// provider.Provider was configured for it, falling back to one default
// when an operation has no specific override. This package makes no
// decisions about *which* provider is good for an operation -- that's
// deployment configuration, resolved once at startup by whoever
// constructs a Router (main.go, once the AI HTTP handlers exist to
// consume it).
package router

import "github.com/cairnobs/cairnobs/api/ai/provider"

type Operation string

const (
	OpTranslate Operation = "translate"
	OpComplete  Operation = "complete"
	OpExplain   Operation = "explain"
	OpFix       Operation = "fix"
)

// Router selects a provider.Provider per Operation. Not itself a
// provider.Provider -- callers ask For(op) and then call the operation
// they actually need on the result, rather than this type trying to
// implement all four methods and dispatch internally, which would just
// be an extra layer of indirection for no benefit.
type Router struct {
	byOp     map[Operation]provider.Provider
	fallback provider.Provider
}

// New builds a Router. fallback must not be nil -- every operation
// resolves to *some* provider, even a deployment that never calls
// SetOperation and just wants one model for everything.
func New(fallback provider.Provider) *Router {
	return &Router{byOp: make(map[Operation]provider.Provider), fallback: fallback}
}

// SetOperation overrides which provider handles op. Call once per
// operation that needs a non-default model at startup configuration
// time -- not intended to change at runtime (a Router isn't
// synchronized for concurrent SetOperation/For calls, matching every
// other "assembled once in main.go, read-only after that" config shape
// in this codebase, e.g. Handler's fields in queryapi).
func (r *Router) SetOperation(op Operation, p provider.Provider) {
	r.byOp[op] = p
}

// For returns the provider configured for op, or the fallback if none
// was set specifically.
func (r *Router) For(op Operation) provider.Provider {
	if p, ok := r.byOp[op]; ok {
		return p
	}
	return r.fallback
}
