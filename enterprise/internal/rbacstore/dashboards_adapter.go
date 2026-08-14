// Adapts *Store to api/dashboards.PermissionStore -- the interface core
// defines and has carried as a nil-by-default field
// (api/dashboards.Handler.permissions) since Phase 4 task 5, waiting on
// exactly this: a real implementation, wired in by
// enterprise/cmd/enterprise-api, the one binary allowed to import both
// packages. Same shape as audit.QueryAPILogger's adapter over
// api/queryapi.AuditLogger.
package rbacstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/dashboards"
)

// DashboardPermissions implements dashboards.PermissionStore by
// translating between authz.Role (core's type) and this package's Role
// (kept separate rather than importing authz's constants directly --
// see Role's own doc comment for why).
type DashboardPermissions struct {
	store *Store
}

func NewDashboardPermissions(store *Store) *DashboardPermissions {
	return &DashboardPermissions{store: store}
}

func (d *DashboardPermissions) GrantedRole(ctx context.Context, dashboardID, userID string) (authz.Role, bool, error) {
	p, err := d.store.GetDashboardPermission(ctx, dashboardID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return authz.Role(p.Role), true, nil
}

func (d *DashboardPermissions) SetPermission(ctx context.Context, dashboardID, userID string, role authz.Role, grantedBy string) error {
	if role != authz.RoleViewer && role != authz.RoleEditor {
		return fmt.Errorf("rbacstore: dashboard permission role must be viewer or editor, got %q", role)
	}
	return d.store.SetDashboardPermission(ctx, dashboardID, userID, Role(role), grantedBy)
}

func (d *DashboardPermissions) RevokePermission(ctx context.Context, dashboardID, userID string) error {
	return d.store.RevokeDashboardPermission(ctx, dashboardID, userID)
}

func (d *DashboardPermissions) ListPermissions(ctx context.Context, dashboardID string) ([]dashboards.Permission, error) {
	rows, err := d.store.ListDashboardPermissions(ctx, dashboardID)
	if err != nil {
		return nil, err
	}
	out := make([]dashboards.Permission, 0, len(rows))
	for _, r := range rows {
		out = append(out, dashboards.Permission{
			UserID: r.UserID, Role: authz.Role(r.Role), GrantedBy: r.GrantedBy, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}
