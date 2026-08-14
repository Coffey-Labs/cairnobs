package rbacstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DashboardPermission is one dashboard_permissions row -- see
// /docs/phase-4-rbac-design.md's "additive-only per-resource grants"
// section and metadata/migrations/0024/0033. Role is always RoleViewer
// or RoleEditor: metadata/migrations/0033_restrict_dashboard_permissions_role.sql
// narrowed the CHECK constraint to match, since Admin/Owner already have
// tenant-wide access and never need a resource-level grant.
type DashboardPermission struct {
	DashboardID string
	UserID      string
	Role        Role
	GrantedBy   string
	CreatedAt   time.Time
}

// SetDashboardPermission upserts a grant -- the sole mutation path,
// same "one method, ON CONFLICT DO UPDATE" shape as SetMembership, so a
// future audit-log hook has one call site to wrap. grantedBy is
// required (metadata/migrations/0033 made granted_by NOT NULL): every
// grant must be attributable to the identity that created it.
func (s *Store) SetDashboardPermission(ctx context.Context, dashboardID, userID string, role Role, grantedBy string) error {
	if grantedBy == "" {
		return fmt.Errorf("rbacstore: grantedBy is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dashboard_permissions (id, dashboard_id, user_id, role, granted_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dashboard_id, user_id) DO UPDATE
			SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by`,
		uuid.NewString(), dashboardID, userID, string(role), grantedBy)
	if err != nil {
		return fmt.Errorf("rbacstore: setting dashboard permission: %w", err)
	}
	return nil
}

func (s *Store) RevokeDashboardPermission(ctx context.Context, dashboardID, userID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM dashboard_permissions WHERE dashboard_id = $1 AND user_id = $2`, dashboardID, userID)
	if err != nil {
		return fmt.Errorf("rbacstore: revoking dashboard permission: %w", err)
	}
	return nil
}

func (s *Store) GetDashboardPermission(ctx context.Context, dashboardID, userID string) (*DashboardPermission, error) {
	var p DashboardPermission
	var role string
	row := s.pool.QueryRow(ctx, `
		SELECT dashboard_id, user_id, role, granted_by, created_at
		FROM dashboard_permissions WHERE dashboard_id = $1 AND user_id = $2`, dashboardID, userID)
	if err := row.Scan(&p.DashboardID, &p.UserID, &role, &p.GrantedBy, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting dashboard permission: %w", err)
	}
	p.Role = Role(role)
	return &p, nil
}

// ListDashboardPermissions supports the "manage a dashboard's per-user
// grants" UI/endpoint -- every grant on one dashboard, for a
// creator/Admin/Owner to review or revoke.
func (s *Store) ListDashboardPermissions(ctx context.Context, dashboardID string) ([]DashboardPermission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dashboard_id, user_id, role, granted_by, created_at
		FROM dashboard_permissions WHERE dashboard_id = $1 ORDER BY created_at`, dashboardID)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing dashboard permissions: %w", err)
	}
	defer rows.Close()

	var out []DashboardPermission
	for rows.Next() {
		var p DashboardPermission
		var role string
		if err := rows.Scan(&p.DashboardID, &p.UserID, &role, &p.GrantedBy, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning dashboard permission: %w", err)
		}
		p.Role = Role(role)
		out = append(out, p)
	}
	return out, rows.Err()
}
