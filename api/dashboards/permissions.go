package dashboards

import (
	"context"
	"time"

	"github.com/cairnobs/cairnobs/api/authz"
)

// Permission is one dashboard_permissions row -- see
// /docs/phase-4-rbac-design.md's "additive-only per-resource grants"
// section. Role is always RoleViewer or RoleEditor: Admin/Owner already
// have tenant-wide access to every dashboard, so a resource-level grant
// only ever needs to raise someone as high as Editor on one dashboard
// (metadata/migrations/0033_restrict_dashboard_permissions_role.sql).
type Permission struct {
	UserID    string
	Role      authz.Role
	GrantedBy string
	CreatedAt time.Time
}

// PermissionStore resolves and manages per-resource dashboard grants --
// the RBAC matrix's "(own/granted)" qualifier that a baseline tenant
// role alone can't answer (see RegisterRoutes' doc comment). nil is a
// deliberate no-op, the same shape as a nil authz.Authorizer: a
// single-tenant deployment, or one running plain api/cmd/api with RBAC
// enforcement on but no enterprise permission service wired, simply
// doesn't get the "granted" half of "(own/granted)" -- ownership and
// Admin/Owner access still work (see canEditDashboard), only grants
// beyond that are unavailable. The real implementation
// (enterprise/internal/rbacstore.DashboardPermissions) is enterprise
// code, wired in only by enterprise/cmd/enterprise-api -- core never
// imports it, per the module boundary.
type PermissionStore interface {
	// GrantedRole returns the role a specific user has been granted on a
	// specific dashboard, ok=false if no grant exists.
	GrantedRole(ctx context.Context, dashboardID, userID string) (role authz.Role, ok bool, err error)
	// SetPermission creates or updates a grant. grantedBy is the
	// authenticated identity performing the grant -- always recorded,
	// never empty, so every grant is attributable (see the migration
	// above's NOT NULL fix).
	SetPermission(ctx context.Context, dashboardID, userID string, role authz.Role, grantedBy string) error
	RevokePermission(ctx context.Context, dashboardID, userID string) error
	ListPermissions(ctx context.Context, dashboardID string) ([]Permission, error)
}

// validGrantRole reports whether role is one a dashboard_permissions row
// may actually hold -- see Permission's doc comment for why Admin/Owner
// are excluded.
func validGrantRole(role authz.Role) bool {
	return role == authz.RoleViewer || role == authz.RoleEditor
}
