// Package rbacstore is the pgx-backed CRUD layer over the tenant/user/
// role schema (metadata/migrations/0017-0021) described in
// /docs/phase-4-rbac-design.md: users (global SSO identity), tenants,
// and tenant_memberships (per-tenant role). It uses the same shared
// "sentry" Postgres role/pool every other metadata store does (unlike
// enterprise/internal/audit's deliberately separate, narrower-granted
// pool) -- ordinary read/write CRUD on control-plane config, not an
// append-only ledger, so it has no analogous reason to restrict its own
// write access.
//
// This package is the storage building block a future OIDC/SAML login
// HTTP handler would call to resolve "which tenant/role does this SSO
// identity map to" and issue a session (internal/session) accordingly --
// that handler itself isn't built yet (see cmd/enterprise-auth/main.go's
// doc comment), so today rbacstore's only production caller is
// -mint-service-token's future tenant-aware successor and its own tests.
// dashboard_permissions and data_sources (also part of the schema) don't
// have CRUD here yet -- no caller needs them until dashboards' handler
// wiring reads per-resource grants, named as deferred in task 5's
// summary.
package rbacstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Get-shaped methods when the row doesn't exist.
var ErrNotFound = errors.New("rbacstore: not found")

type User struct {
	ID          string
	Email       string
	DisplayName string
	SSOSubject  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Tenant struct {
	ID              string
	DisplayName     string
	Status          string
	OwnerUserID     string // empty until a first Owner is assigned
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Role mirrors api/internal/authz.Role's string values, kept as a plain
// string here rather than importing authz -- rbacstore is enterprise
// code and api/internal/authz is core; enterprise may depend on
// nothing-shaped-like-an-import-from-core per the module boundary
// (see /docs/phase-4-isolation-design.md), even though the reverse
// (core importing enterprise) is the one hack/check-tenant-boundary.sh
// actually enforces. Values must stay in sync with authz.Role's
// constants by convention, verified by rbacstore_test.go.
type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

type Membership struct {
	TenantID string
	UserID   string
	Role     Role
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertUserBySSO finds an existing user by ssoSubject, falling back to
// email (covers a user pre-provisioned by an Admin before their first
// SSO login -- see 0017_create_users.sql's ssoSubject nullability
// comment), or creates a new row. This is the one place a user's
// display_name/ssoSubject are refreshed from IdP claims on every login,
// matching a typical SSO-managed-identity pattern (the IdP is the
// source of truth for name/email; role assignment stays local, per
// /docs/phase-4-rbac-design.md's "manual role assignment" baseline).
func (s *Store) UpsertUserBySSO(ctx context.Context, ssoSubject, email, displayName string) (*User, error) {
	if ssoSubject == "" || email == "" {
		return nil, fmt.Errorf("rbacstore: ssoSubject and email are required")
	}

	var u User
	row := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, email, display_name, sso_subject)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
			SET display_name = EXCLUDED.display_name,
			    sso_subject  = EXCLUDED.sso_subject,
			    updated_at   = now()
		RETURNING id, email, display_name, sso_subject, created_at, updated_at`,
		uuid.NewString(), email, displayName, ssoSubject)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.SSOSubject, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, fmt.Errorf("rbacstore: upserting user: %w", err)
	}
	return &u, nil
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	row := s.pool.QueryRow(ctx, `
		SELECT id, email, display_name, sso_subject, created_at, updated_at
		FROM users WHERE id = $1`, id)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.SSOSubject, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting user: %w", err)
	}
	return &u, nil
}

// CreateTenant inserts a new tenant in 'provisioning' status -- callers
// (future tenant-provisioning code, per /docs/phase-4-isolation-design.md's
// ordered provisioning state machine) move it to 'active' via
// SetTenantStatus only after ClickHouse/Tantivy provisioning succeeds.
func (s *Store) CreateTenant(ctx context.Context, id, displayName string) (*Tenant, error) {
	if id == "" || displayName == "" {
		return nil, fmt.Errorf("rbacstore: id and displayName are required")
	}
	var t Tenant
	row := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, display_name, status)
		VALUES ($1, $2, 'provisioning')
		RETURNING id, display_name, status, coalesce(owner_user_id::text, ''), created_at, updated_at`,
		id, displayName)
	if err := row.Scan(&t.ID, &t.DisplayName, &t.Status, &t.OwnerUserID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, fmt.Errorf("rbacstore: creating tenant: %w", err)
	}
	return &t, nil
}

func (s *Store) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	var t Tenant
	row := s.pool.QueryRow(ctx, `
		SELECT id, display_name, status, coalesce(owner_user_id::text, ''), created_at, updated_at
		FROM tenants WHERE id = $1`, id)
	if err := row.Scan(&t.ID, &t.DisplayName, &t.Status, &t.OwnerUserID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting tenant: %w", err)
	}
	return &t, nil
}

// SetTenantStatus is the only way a tenant's status column changes --
// every tenant-resolution path elsewhere must re-check this via
// GetTenant, never cache/assume 'active', per
// /docs/phase-4-isolation-design.md's provisioning gate.
func (s *Store) SetTenantStatus(ctx context.Context, id, status string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tenants SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("rbacstore: setting tenant status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetOwner sets a tenant's owner_user_id -- separate from
// SetMembership because the schema's Owner is a tenant-level column
// (exactly one, non-removable except by itself/platform break-glass per
// /docs/phase-4-rbac-design.md), not just the highest tenant_memberships
// role. Callers are expected to also call SetMembership(tenantID,
// userID, RoleOwner) so the membership table and this column agree --
// this package doesn't wrap both in one method because tenant creation
// (no owner yet) and ownership transfer (existing owner changes) are
// different call sites with different validation needs.
func (s *Store) SetOwner(ctx context.Context, tenantID, userID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tenants SET owner_user_id = $2, updated_at = now() WHERE id = $1`, tenantID, userID)
	if err != nil {
		return fmt.Errorf("rbacstore: setting tenant owner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMembership upserts a user's role for a tenant -- the sole mutation
// path for tenant_memberships, so every role change naturally funnels
// through one method a future audit-log hook (EventRoleChange, see
// enterprise/internal/audit) can wrap.
func (s *Store) SetMembership(ctx context.Context, tenantID, userID string, role Role) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id) DO UPDATE
			SET role = EXCLUDED.role, updated_at = now()`,
		uuid.NewString(), tenantID, userID, string(role))
	if err != nil {
		return fmt.Errorf("rbacstore: setting membership: %w", err)
	}
	return nil
}

func (s *Store) GetMembership(ctx context.Context, tenantID, userID string) (*Membership, error) {
	var m Membership
	var role string
	row := s.pool.QueryRow(ctx, `
		SELECT tenant_id, user_id, role FROM tenant_memberships
		WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err := row.Scan(&m.TenantID, &m.UserID, &role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting membership: %w", err)
	}
	m.Role = Role(role)
	return &m, nil
}

// ListMembershipsForUser supports "which tenants can this user act in,
// and at what role" -- the shape a login/session-issuance handler needs
// when a user belongs to more than one tenant and must pick (or be
// asked to pick) which one to act as for a given session.
func (s *Store) ListMembershipsForUser(ctx context.Context, userID string) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, user_id, role FROM tenant_memberships WHERE user_id = $1 ORDER BY tenant_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing memberships: %w", err)
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		var role string
		if err := rows.Scan(&m.TenantID, &m.UserID, &role); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning membership: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}
