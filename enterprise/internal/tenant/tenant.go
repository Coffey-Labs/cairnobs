// Package tenant is the single source of truth for "which tenant is
// this request for" -- see /docs/phase-4-isolation-design.md's "TenantID:
// an honest framing, not an oversold one" section before changing
// anything here.
//
// The unexported field on ID and the single production constructor make
// *accidental* misuse cheap to audit (grep for call sites) -- they do
// not make misuse impossible by the Go compiler alone. The real
// invariant: TrustFromValidatedSession has exactly one production call
// site, verified by CI (hack/check-tenant-boundary.sh) and code review
// at every change to this package. The database/index grant layer in
// internal/chrunner and internal/searchclient is the actual backstop.
// Do not add a second exported or reflection-accessible construction
// path (e.g. an UnmarshalJSON method) without re-reading that design
// doc section first -- it exists specifically because a future
// "convenience" constructor is the most realistic way this boundary
// gets quietly reopened.
package tenant

import "context"

// ID identifies a tenant. The zero value is not a valid ID -- always
// check the bool from FromContext.
type ID struct {
	value string
}

func (id ID) String() string {
	return id.value
}

// contextKey is unexported specifically so nothing outside this package
// can set or shadow the context value via context.WithValue with a
// string or exported key -- see the design doc's "context key collision"
// gap.
type contextKey struct{}

// FromContext is the only read path for a request's tenant.
func FromContext(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(contextKey{}).(ID)
	return id, ok
}

// WithContext attaches id to ctx. Called once, by auth middleware, right
// after TrustFromValidatedSession.
func WithContext(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// TrustFromValidatedSession is the only construction path from a raw
// string. In production code, DO NOT CALL OUTSIDE auth middleware
// (internal/session) -- enforced by hack/check-tenant-boundary.sh, which
// greps *.go files (excluding _test.go) for call sites outside an
// allowlist. Other packages' tests calling this directly is expected and
// fine: test code isn't attacker-controlled the way a network-facing
// handler is, so there's no separate "test constructor" here -- an
// earlier draft of this design proposed one living in a _test.go file,
// on the mistaken assumption that would make it importable by other
// packages' tests as a compiler-enforced guarantee. It doesn't: Go never
// compiles _test.go files into what other packages (or other packages'
// tests) import, so a same-package-only test constructor would have been
// unreachable from anywhere outside this package, including its
// intended callers. This function, called directly, is simpler and
// actually works.
func TrustFromValidatedSession(raw string) ID {
	return ID{value: raw}
}
