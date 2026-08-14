// Package authz is core's RBAC extension point -- deliberately minimal
// and tenant-agnostic in shape, matching queryapi.AuditLogger's pattern:
// core defines the interface and the types, enterprise/ supplies the
// implementation. Unlike AuditLogger, the implementation Authorizer
// wires in here is NOT enterprise Go code injected directly (that would
// require api to import enterprise/, violating the module boundary
// confirmed in /docs/phase-4-isolation-design.md) -- it's an HTTP client
// to enterprise-auth's /internal/authorize endpoint (see httpauthz.go),
// the same "network boundary, not import boundary" pattern already
// established between /api and /alerting.
package authz

import (
	"context"
	"net/http"
)

// Role is ordered for human roles (Viewer < Editor < Admin < Owner, per
// /docs/phase-4-rbac-design.md) plus a separate, non-comparable Service
// lane for machine callers like /alerting's evaluator -- see Satisfies.
type Role string

const (
	RoleViewer  Role = "viewer"
	RoleEditor  Role = "editor"
	RoleAdmin   Role = "admin"
	RoleOwner   Role = "owner"
	RoleService Role = "service"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3, RoleOwner: 4}

// Satisfies reports whether this role meets a requirement. RoleService
// only ever satisfies RoleService -- a service credential never
// satisfies a human-role requirement, and a human role never satisfies
// a RoleService requirement, by design: /docs/phase-4-isolation-design.md's
// alerting service identity is deliberately not a point on the human
// role scale, so it can't accidentally inherit broader access by
// ranking above Viewer.
func (r Role) Satisfies(required Role) bool {
	if required == RoleService || r == RoleService {
		return r == required
	}
	return roleRank[r] >= roleRank[required]
}

// Identity is what a successful Authorize call resolves. UserID is
// empty for RoleService (see /docs/phase-4-isolation-design.md's
// alerting↔api gap -- a service credential proves "this caller is
// alerting," not "this caller is acting as a specific human").
type Identity struct {
	TenantID string
	UserID   string
	Role     Role
}

// Authorizer resolves an Identity from an incoming request's
// credentials (session cookie or service token) without knowing
// anything about *what* the caller is trying to do -- permission
// checking against a required Role happens in the middleware
// (middleware.go), not here, so this interface stays a pure
// "who is this" question.
type Authorizer interface {
	Authorize(r *http.Request) (Identity, error)
}

type identityContextKey struct{}

// IdentityFromContext lets a handler read the resolved identity a
// RequireRole/RequireRoleOrService middleware attached -- e.g. to
// populate QueryAuditEntry's tenant/user once Phase 4's audit wiring
// threads identity through (see queryapi.AuditLogger's doc comment).
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an already-resolved Identity to ctx -- exported
// (not just middleware.go's internal use) so packages that construct
// their own request context outside an HTTP handler -- e.g. enterprise/
// internal/chrunner's tests, or a future non-HTTP caller -- can put a
// real Identity in context the same way RequireRole/RequireRoleOrService
// do, rather than reaching for an unexported field via reflection or
// duplicating this one-line function.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}
