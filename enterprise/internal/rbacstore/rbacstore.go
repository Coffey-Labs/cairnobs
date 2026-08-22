// Package rbacstore is the pgx-backed CRUD layer over the tenant/user/
// role schema (metadata/migrations/0017-0021) described in
// /docs/phase-4-rbac-design.md: users (global SSO identity), tenants,
// and tenant_memberships (per-tenant role). It uses the same shared
// "cairnobs" Postgres role/pool every other metadata store does (unlike
// enterprise/internal/audit's deliberately separate, narrower-granted
// pool) -- ordinary read/write CRUD on control-plane config, not an
// append-only ledger, so it has no analogous reason to restrict its own
// write access.
//
// This package is the storage building block internal/loginhandler's
// OIDC/SAML handlers call to resolve "which tenant/role does this SSO
// identity map to" and issue a session (internal/session) accordingly.
// dashboard_permissions CRUD (dashboard_permissions.go) is wrapped by
// DashboardPermissions (dashboards_adapter.go) to implement
// api/dashboards.PermissionStore -- see that adapter's doc comment.
// data_sources CRUD was added once enterprise/internal/chrunner needed a
// real place to read per-tenant ClickHouse credentials from at startup
// (see that package's doc comment).
package rbacstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	ID          string
	DisplayName string
	Status      string
	OwnerUserID string // empty until a first Owner is assigned
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Role mirrors api/authz.Role's string values, kept as a plain
// string here rather than importing authz -- rbacstore is enterprise
// code and api/authz is core; enterprise may depend on
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

// GetUserByEmail supports enterprise-auth's -grant-membership-* operator
// flags (cmd/enterprise-auth/main.go): granting a tenant_memberships row
// by email is friendlier than requiring the operator to already know a
// user's generated UUID, and email is the same natural key
// UpsertUserBySSO already upserts on.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	row := s.pool.QueryRow(ctx, `
		SELECT id, email, display_name, sso_subject, created_at, updated_at
		FROM users WHERE email = $1`, email)
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.SSOSubject, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting user by email: %w", err)
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

// TenantIsActive answers a narrow, frequently-asked question -- it
// implements enterprise/internal/searchclient.TenantChecker
// structurally (no import needed in that direction; see that package's
// doc comment for why Tantivy's per-tenant index resolution needs this
// check where chrunner's ClickHouse routing gets the equivalent
// guarantee for free from its immutable startup-built connection map).
// Backed by GetTenant, never cached -- SetTenantStatus's doc comment
// already establishes "re-check server-side, never assume 'active'" as
// this package's convention.
func (s *Store) TenantIsActive(ctx context.Context, tenantID string) (bool, error) {
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return t.Status == "active", nil
}

// ListActiveTenantIDs backs enterprise-auth's GET /internal/active-tenants
// -- search (AGPL core) polls this to learn which tenant_ids it may
// safely write-route into their own Tantivy index, since it has no
// Postgres access of its own (see search/src/tenants.rs's doc comment
// for the full design). Deliberately narrower than
// ListProvisionedDataSources (which also requires ClickHouse
// credentials to be present, since chwriter.Registry needs those to
// actually connect): search's write path needs nothing but the id.
func (s *Store) ListActiveTenantIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM tenants WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing active tenant ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning active tenant id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbacstore: iterating active tenant ids: %w", err)
	}
	return ids, nil
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

// TransferOwner atomically moves a tenant's Owner from whoever currently
// holds tenants.owner_user_id to newOwnerUserID: downgrades the
// current owner's tenant_memberships row to downgradeRole (the RBAC
// matrix's "Transfer tenant Owner -- Owner only" -- the previous owner
// keeps a real role, typically RoleAdmin, rather than being silently
// ejected from the tenant), upserts the new owner's membership to
// RoleOwner, and updates tenants.owner_user_id -- all in one
// transaction, the first in this package. Every other mutation here is
// a single independent statement because nothing else needs more than
// one row to agree; this genuinely does, for the exact reason
// RevokeMembership's doc comment already gives: owner_user_id pointing
// at a user whose tenant_memberships row doesn't say 'owner' (or vice
// versa) is an inconsistent state, and calling SetOwner/SetMembership
// separately outside a transaction risks landing in it on any failure
// between the two calls, not just caller error.
func (s *Store) TransferOwner(ctx context.Context, tenantID, newOwnerUserID string, downgradeRole Role) error {
	tenant, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("rbacstore: getting tenant to transfer ownership: %w", err)
	}
	if tenant.OwnerUserID == "" {
		return fmt.Errorf("rbacstore: tenant %q has no current owner -- use SetMembership+SetOwner directly for a first assignment, TransferOwner is for an existing owner handing off", tenantID)
	}
	if tenant.OwnerUserID == newOwnerUserID {
		return fmt.Errorf("rbacstore: %q is already tenant %q's owner", newOwnerUserID, tenantID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rbacstore: beginning ownership transfer: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit succeeds

	upsertMembership := `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id) DO UPDATE
			SET role = EXCLUDED.role, updated_at = now()`
	if _, err := tx.Exec(ctx, upsertMembership, uuid.NewString(), tenantID, tenant.OwnerUserID, string(downgradeRole)); err != nil {
		return fmt.Errorf("rbacstore: downgrading previous owner's membership: %w", err)
	}
	if _, err := tx.Exec(ctx, upsertMembership, uuid.NewString(), tenantID, newOwnerUserID, string(RoleOwner)); err != nil {
		return fmt.Errorf("rbacstore: setting new owner's membership: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE tenants SET owner_user_id = $2, updated_at = now() WHERE id = $1`, tenantID, newOwnerUserID)
	if err != nil {
		return fmt.Errorf("rbacstore: setting tenant owner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbacstore: committing ownership transfer: %w", err)
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

// RevokeMembership deletes a user's membership in a tenant -- the
// counterpart to SetMembership's upsert. Refuses to revoke a tenant's
// current Owner: unlike every other role, Owner is also a dedicated
// tenants.owner_user_id column (see SetOwner's doc comment), so
// revoking that membership without first transferring ownership would
// leave owner_user_id pointing at a user with no membership in the
// tenant at all -- an inconsistent state, not something this method
// silently allows. Ownership transfer is a deliberate, separate action
// (the RBAC matrix's "Transfer tenant Owner -- Owner only"), not a
// side effect of revoking a membership.
func (s *Store) RevokeMembership(ctx context.Context, tenantID, userID string) error {
	tenant, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("rbacstore: getting tenant to check ownership: %w", err)
	}
	if tenant.OwnerUserID == userID {
		return fmt.Errorf("rbacstore: refusing to revoke tenant %q's current Owner (%s) -- transfer ownership first", tenantID, userID)
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return fmt.Errorf("rbacstore: revoking membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TenantMember is one row of ListMembershipsForTenant's result -- joined
// with users so a caller (e.g. enterprise-auth's -list-memberships-tenant
// operator flag) can show something more useful than a bare user ID.
type TenantMember struct {
	UserID      string
	Email       string
	DisplayName string
	Role        Role
}

// ListMembershipsForTenant is ListMembershipsForUser's inverse -- "who
// is in this tenant, and at what role," the shape an admin reviewing or
// revoking access needs. Joined with users (INNER, not LEFT: a
// tenant_memberships row's user_id is NOT NULL and FK-constrained, so
// every membership has a real user).
func (s *Store) ListMembershipsForTenant(ctx context.Context, tenantID string) ([]TenantMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.display_name, m.role
		FROM tenant_memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1
		ORDER BY u.email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing tenant members: %w", err)
	}
	defer rows.Close()

	var out []TenantMember
	for rows.Next() {
		var m TenantMember
		var role string
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &role); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning tenant member: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
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

// MembershipWithTenant is ListMembershipsWithTenantForUser's result row
// -- Membership plus the tenant's display name, the shape a
// tenant-picker UI needs (a bare tenant_id/Role isn't enough to show a
// human something recognizable to choose between).
type MembershipWithTenant struct {
	TenantID          string
	TenantDisplayName string
	Role              Role
}

// ListMembershipsWithTenantForUser is ListMembershipsForUser plus a join
// against tenants -- used by loginhandler's multi-membership
// tenant-selection step (GET /auth/memberships), which is the one
// caller that actually needs to show a human "here are your tenants,"
// not just resolve a single membership programmatically.
func (s *Store) ListMembershipsWithTenantForUser(ctx context.Context, userID string) ([]MembershipWithTenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.tenant_id, t.display_name, m.role
		FROM tenant_memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1
		ORDER BY t.display_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing memberships with tenant: %w", err)
	}
	defer rows.Close()

	var out []MembershipWithTenant
	for rows.Next() {
		var m MembershipWithTenant
		var role string
		if err := rows.Scan(&m.TenantID, &m.TenantDisplayName, &role); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning membership with tenant: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DataSource is one tenant's data-plane location -- today, exactly one
// ClickHouse database + one Tantivy index per tenant (see
// /docs/phase-4-rbac-design.md's "data_sources" extension-point
// section). ClickHouseUsername/Password are nil until
// enterprise/internal/tenantprovision actually provisions the
// ClickHouse-side user/database and calls SetDataSourceClickHouseCredentials.
type DataSource struct {
	ID                     string
	TenantID               string
	Name                   string
	ClickHouseDatabaseName string
	TantivyIndexPath       string
	ClickHouseUsername     *string
	ClickHousePassword     *string
}

// CreateDataSource inserts the row tenantprovision will later attach
// credentials to (SetDataSourceClickHouseCredentials) -- split into two
// steps because the row (database name, index path) is decided before
// provisioning runs, but the ClickHouse-side username/password only
// exist after CREATE USER actually succeeds.
func (s *Store) CreateDataSource(ctx context.Context, tenantID, name, clickHouseDatabaseName, tantivyIndexPath string) (*DataSource, error) {
	ds := DataSource{
		ID: uuid.NewString(), TenantID: tenantID, Name: name,
		ClickHouseDatabaseName: clickHouseDatabaseName, TantivyIndexPath: tantivyIndexPath,
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO data_sources (id, tenant_id, name, clickhouse_database_name, tantivy_index_path)
		VALUES ($1, $2, $3, $4, $5)`,
		ds.ID, ds.TenantID, ds.Name, ds.ClickHouseDatabaseName, ds.TantivyIndexPath)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: creating data source: %w", err)
	}
	return &ds, nil
}

// SetDataSourceClickHouseCredentials is the only way
// clickhouse_username/password change -- called once, right after
// enterprise/internal/tenantprovision.ProvisionClickHouse succeeds.
// Never called again for the same data source: rotating a live tenant's
// credential without first updating it on the ClickHouse side would
// just break every open connection, same reasoning as
// deploy/operator/internal/controller/tenant_controller.go's
// reconcileSecret.
func (s *Store) SetDataSourceClickHouseCredentials(ctx context.Context, id, username, password string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE data_sources SET clickhouse_username = $2, clickhouse_password = $3 WHERE id = $1`,
		id, username, password)
	if err != nil {
		// A malformed id (not valid UUID syntax) can never match a row
		// either way -- treat it the same as "no such row" rather than
		// leaking Postgres's raw 22P02 error past this store's boundary.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return ErrNotFound
		}
		return fmt.Errorf("rbacstore: setting data source credentials: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDataSource(row pgx.Row) (*DataSource, error) {
	var ds DataSource
	if err := row.Scan(&ds.ID, &ds.TenantID, &ds.Name, &ds.ClickHouseDatabaseName, &ds.TantivyIndexPath,
		&ds.ClickHouseUsername, &ds.ClickHousePassword); err != nil {
		return nil, err
	}
	return &ds, nil
}

func (s *Store) GetDataSourceForTenant(ctx context.Context, tenantID string) (*DataSource, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, clickhouse_database_name, tantivy_index_path, clickhouse_username, clickhouse_password
		FROM data_sources WHERE tenant_id = $1 ORDER BY created_at LIMIT 1`, tenantID)
	ds, err := scanDataSource(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("rbacstore: getting data source: %w", err)
	}
	return ds, nil
}

// ListProvisionedDataSources returns every data source for an active
// tenant that has already been provisioned (ClickHouse credentials set)
// -- exactly the set enterprise/internal/chrunner.NewRegistry needs at
// startup. A data source with no credentials yet (tenantprovision hasn't
// run for it) is deliberately excluded rather than returned with empty
// credentials -- chrunner has nothing safe to connect with for it, and
// silently including it would turn into a confusing empty-string
// connection attempt instead of a clear "not provisioned yet" absence.
func (s *Store) ListProvisionedDataSources(ctx context.Context) ([]DataSource, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ds.id, ds.tenant_id, ds.name, ds.clickhouse_database_name, ds.tantivy_index_path,
		       ds.clickhouse_username, ds.clickhouse_password
		FROM data_sources ds
		JOIN tenants t ON t.id = ds.tenant_id
		WHERE t.status = 'active' AND ds.clickhouse_username IS NOT NULL AND ds.clickhouse_password IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing provisioned data sources: %w", err)
	}
	defer rows.Close()

	var out []DataSource
	for rows.Next() {
		ds, err := scanDataSource(rows)
		if err != nil {
			return nil, fmt.Errorf("rbacstore: scanning data source: %w", err)
		}
		out = append(out, *ds)
	}
	return out, rows.Err()
}
