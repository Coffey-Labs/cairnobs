// Integration tests against a real Postgres -- rbacstore's whole job is
// SQL (upserts, FK constraints, unique constraints on
// (tenant_id, user_id)/(dashboard_id, user_id)), so a mocked pool
// wouldn't actually exercise it. Skipped unless RBACSTORE_TEST_POSTGRES_ADDR
// is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e RBACSTORE_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e RBACSTORE_TEST_POSTGRES_PASSWORD=sentry-dev-only \
//	  golang:1.25-alpine go test ./internal/rbacstore/... -v
package rbacstore

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/authz"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("RBACSTORE_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("RBACSTORE_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("RBACSTORE_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://sentry:%s@%s/sentry_metadata", password, addr)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

// uniqueTestTenant/uniqueTestEmail avoid collisions across repeated test
// runs against a persistent dev Postgres (no cleanup step deletes rows,
// unlike audit's cleanupAuditLog -- these rows are meant to look like
// real, retained control-plane data, not scratch state).
func uniqueSuffix() string {
	return uuid.NewString()[:8]
}

func TestCreateAndGetTenant(t *testing.T) {
	s := testStore(t)
	id := "test-tenant-" + uniqueSuffix()

	created, err := s.CreateTenant(context.Background(), id, "Test Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if created.Status != "provisioning" {
		t.Fatalf("new tenant status = %q, want provisioning", created.Status)
	}
	if created.OwnerUserID != "" {
		t.Fatalf("new tenant owner = %q, want empty until an Owner is assigned", created.OwnerUserID)
	}

	got, err := s.GetTenant(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.DisplayName != "Test Tenant" {
		t.Fatalf("DisplayName = %q, want %q", got.DisplayName, "Test Tenant")
	}
}

func TestGetTenantNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetTenant(context.Background(), "does-not-exist-"+uniqueSuffix()); err != ErrNotFound {
		t.Fatalf("GetTenant error = %v, want ErrNotFound", err)
	}
}

func TestSetTenantStatusGatesProvisioning(t *testing.T) {
	s := testStore(t)
	id := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(context.Background(), id, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.SetTenantStatus(context.Background(), id, "active"); err != nil {
		t.Fatalf("SetTenantStatus: %v", err)
	}
	got, err := s.GetTenant(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("Status = %q, want active", got.Status)
	}
}

func TestSetTenantStatusNotFound(t *testing.T) {
	s := testStore(t)
	if err := s.SetTenantStatus(context.Background(), "does-not-exist-"+uniqueSuffix(), "active"); err != ErrNotFound {
		t.Fatalf("SetTenantStatus error = %v, want ErrNotFound", err)
	}
}

func TestUpsertUserBySSOCreatesThenUpdates(t *testing.T) {
	s := testStore(t)
	email := "user-" + uniqueSuffix() + "@example.com"

	u1, err := s.UpsertUserBySSO(context.Background(), "sub-1", email, "First Name")
	if err != nil {
		t.Fatalf("UpsertUserBySSO (create): %v", err)
	}
	if u1.SSOSubject != "sub-1" || u1.DisplayName != "First Name" {
		t.Fatalf("unexpected user: %+v", u1)
	}

	// Second call with the same email (as if the IdP changed the
	// display name, or re-issued a new "sub") must update the same row,
	// not create a second one -- email is the natural key here.
	u2, err := s.UpsertUserBySSO(context.Background(), "sub-2", email, "Updated Name")
	if err != nil {
		t.Fatalf("UpsertUserBySSO (update): %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("upsert created a second row: first ID %q, second ID %q", u1.ID, u2.ID)
	}
	if u2.SSOSubject != "sub-2" || u2.DisplayName != "Updated Name" {
		t.Fatalf("upsert did not refresh IdP-sourced fields: %+v", u2)
	}
}

func TestSetOwnerAndMembershipRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	email := "owner-" + uniqueSuffix() + "@example.com"

	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	user, err := s.UpsertUserBySSO(ctx, "sub-owner", email, "Owner")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}

	if err := s.SetMembership(ctx, tenantID, user.ID, RoleOwner); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	if err := s.SetOwner(ctx, tenantID, user.ID); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	tenant, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if tenant.OwnerUserID != user.ID {
		t.Fatalf("tenant OwnerUserID = %q, want %q", tenant.OwnerUserID, user.ID)
	}

	membership, err := s.GetMembership(ctx, tenantID, user.ID)
	if err != nil {
		t.Fatalf("GetMembership: %v", err)
	}
	if membership.Role != RoleOwner {
		t.Fatalf("membership role = %q, want owner", membership.Role)
	}

	// Re-setting the membership (e.g. a role change) must update in
	// place, not create a duplicate row for the same (tenant, user).
	if err := s.SetMembership(ctx, tenantID, user.ID, RoleAdmin); err != nil {
		t.Fatalf("SetMembership (update): %v", err)
	}
	membership, err = s.GetMembership(ctx, tenantID, user.ID)
	if err != nil {
		t.Fatalf("GetMembership after update: %v", err)
	}
	if membership.Role != RoleAdmin {
		t.Fatalf("membership role after update = %q, want admin", membership.Role)
	}
}

func TestGetMembershipNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	user, err := s.UpsertUserBySSO(ctx, "sub-no-membership", "no-membership-"+uniqueSuffix()+"@example.com", "Nobody")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}
	if _, err := s.GetMembership(ctx, tenantID, user.ID); err != ErrNotFound {
		t.Fatalf("GetMembership error = %v, want ErrNotFound", err)
	}
}

func TestListMembershipsForUserAcrossTenants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user, err := s.UpsertUserBySSO(ctx, "sub-multi", "multi-"+uniqueSuffix()+"@example.com", "Multi Tenant User")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}

	tenantA := "test-tenant-a-" + uniqueSuffix()
	tenantB := "test-tenant-b-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantA, "Tenant A"); err != nil {
		t.Fatalf("CreateTenant A: %v", err)
	}
	if _, err := s.CreateTenant(ctx, tenantB, "Tenant B"); err != nil {
		t.Fatalf("CreateTenant B: %v", err)
	}
	if err := s.SetMembership(ctx, tenantA, user.ID, RoleViewer); err != nil {
		t.Fatalf("SetMembership A: %v", err)
	}
	if err := s.SetMembership(ctx, tenantB, user.ID, RoleAdmin); err != nil {
		t.Fatalf("SetMembership B: %v", err)
	}

	memberships, err := s.ListMembershipsForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListMembershipsForUser: %v", err)
	}
	if len(memberships) != 2 {
		t.Fatalf("got %d memberships, want 2: %+v", len(memberships), memberships)
	}
}

func TestCreateDataSourceThenSetCredentials(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	ds, err := s.CreateDataSource(ctx, tenantID, "default", tenantID, "/var/lib/sentry-search/tenants/"+tenantID)
	if err != nil {
		t.Fatalf("CreateDataSource: %v", err)
	}
	if ds.ClickHouseUsername != nil || ds.ClickHousePassword != nil {
		t.Fatalf("new data source must have no credentials yet, got %+v", ds)
	}

	got, err := s.GetDataSourceForTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetDataSourceForTenant: %v", err)
	}
	if got.ID != ds.ID || got.ClickHouseUsername != nil {
		t.Fatalf("unexpected data source: %+v", got)
	}

	if err := s.SetDataSourceClickHouseCredentials(ctx, ds.ID, "tenant_"+tenantID, "secret-password"); err != nil {
		t.Fatalf("SetDataSourceClickHouseCredentials: %v", err)
	}
	got, err = s.GetDataSourceForTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetDataSourceForTenant after credentials set: %v", err)
	}
	if got.ClickHouseUsername == nil || *got.ClickHouseUsername != "tenant_"+tenantID {
		t.Fatalf("ClickHouseUsername = %v, want tenant_%s", got.ClickHouseUsername, tenantID)
	}
	if got.ClickHousePassword == nil || *got.ClickHousePassword != "secret-password" {
		t.Fatalf("ClickHousePassword = %v, want secret-password", got.ClickHousePassword)
	}
}

func TestSetDataSourceClickHouseCredentialsNotFound(t *testing.T) {
	s := testStore(t)
	if err := s.SetDataSourceClickHouseCredentials(context.Background(), "does-not-exist-"+uniqueSuffix(), "u", "p"); err != ErrNotFound {
		t.Fatalf("SetDataSourceClickHouseCredentials error = %v, want ErrNotFound", err)
	}
}

func TestGetDataSourceForTenantNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetDataSourceForTenant(context.Background(), "does-not-exist-"+uniqueSuffix()); err != ErrNotFound {
		t.Fatalf("GetDataSourceForTenant error = %v, want ErrNotFound", err)
	}
}

// TestListProvisionedDataSourcesExcludesUnprovisionedAndInactive proves
// the two filters ListProvisionedDataSources documents: a data source
// with no ClickHouse credentials yet is excluded (nothing safe to
// connect with), and a data source belonging to a non-active tenant is
// excluded too (a suspended/provisioning tenant must not show up in
// chrunner's connection registry).
func TestListProvisionedDataSourcesExcludesUnprovisionedAndInactive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	activeProvisioned := "test-tenant-" + uniqueSuffix()
	activeUnprovisioned := "test-tenant-" + uniqueSuffix()
	suspended := "test-tenant-" + uniqueSuffix()

	for _, id := range []string{activeProvisioned, activeUnprovisioned, suspended} {
		if _, err := s.CreateTenant(ctx, id, id); err != nil {
			t.Fatalf("CreateTenant %s: %v", id, err)
		}
	}
	if err := s.SetTenantStatus(ctx, activeProvisioned, "active"); err != nil {
		t.Fatalf("SetTenantStatus activeProvisioned: %v", err)
	}
	if err := s.SetTenantStatus(ctx, activeUnprovisioned, "active"); err != nil {
		t.Fatalf("SetTenantStatus activeUnprovisioned: %v", err)
	}
	// suspended stays in 'provisioning' (CreateTenant's default) -- not active.

	dsProvisioned, err := s.CreateDataSource(ctx, activeProvisioned, "default", activeProvisioned, "/idx")
	if err != nil {
		t.Fatalf("CreateDataSource activeProvisioned: %v", err)
	}
	if err := s.SetDataSourceClickHouseCredentials(ctx, dsProvisioned.ID, "u", "p"); err != nil {
		t.Fatalf("SetDataSourceClickHouseCredentials: %v", err)
	}

	if _, err := s.CreateDataSource(ctx, activeUnprovisioned, "default", activeUnprovisioned, "/idx"); err != nil {
		t.Fatalf("CreateDataSource activeUnprovisioned: %v", err)
	}

	dsSuspended, err := s.CreateDataSource(ctx, suspended, "default", suspended, "/idx")
	if err != nil {
		t.Fatalf("CreateDataSource suspended: %v", err)
	}
	if err := s.SetDataSourceClickHouseCredentials(ctx, dsSuspended.ID, "u", "p"); err != nil {
		t.Fatalf("SetDataSourceClickHouseCredentials suspended: %v", err)
	}

	list, err := s.ListProvisionedDataSources(ctx)
	if err != nil {
		t.Fatalf("ListProvisionedDataSources: %v", err)
	}
	foundOurs := false
	for _, ds := range list {
		if ds.TenantID == activeUnprovisioned {
			t.Fatalf("unprovisioned data source leaked into the list: %+v", ds)
		}
		if ds.TenantID == suspended {
			t.Fatalf("non-active tenant's data source leaked into the list: %+v", ds)
		}
		if ds.TenantID == activeProvisioned {
			foundOurs = true
		}
	}
	if !foundOurs {
		t.Fatal("expected the active, provisioned data source to be in the list")
	}
}

// createTestDashboard inserts directly into the dashboards table (owned
// by api/dashboards, not this package) -- dashboard_permissions.
// dashboard_id has a real foreign-key constraint
// (metadata/migrations/0024), so a permission row for a dashboard that
// doesn't exist is rejected by Postgres itself. Mirrors
// api/dashboards/store_integration_test.go's createTestTenant, which
// does the same thing in reverse (inserting into tenants, a table that
// package doesn't own either).
func createTestDashboard(t *testing.T, s *Store, tenantID, createdBy string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO dashboards (id, tenant_id, name, default_earliest, default_latest, created_by)
		VALUES ($1, $2, $3, '-1h', 'now', $4)`, id, tenantID, "Test Dashboard "+uniqueSuffix(), createdBy)
	if err != nil {
		t.Fatalf("inserting test dashboard: %v", err)
	}
	return id
}

func TestSetDashboardPermissionThenGet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	creator, err := s.UpsertUserBySSO(ctx, "sub-creator-"+uniqueSuffix(), "creator-"+uniqueSuffix()+"@example.com", "Creator")
	if err != nil {
		t.Fatalf("UpsertUserBySSO creator: %v", err)
	}
	grantee, err := s.UpsertUserBySSO(ctx, "sub-grantee-"+uniqueSuffix(), "grantee-"+uniqueSuffix()+"@example.com", "Grantee")
	if err != nil {
		t.Fatalf("UpsertUserBySSO grantee: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, creator.ID)

	if err := s.SetDashboardPermission(ctx, dashboardID, grantee.ID, RoleEditor, creator.ID); err != nil {
		t.Fatalf("SetDashboardPermission: %v", err)
	}
	got, err := s.GetDashboardPermission(ctx, dashboardID, grantee.ID)
	if err != nil {
		t.Fatalf("GetDashboardPermission: %v", err)
	}
	if got.Role != RoleEditor || got.GrantedBy != creator.ID {
		t.Fatalf("unexpected permission: %+v", got)
	}

	// Re-setting (e.g. a role change from viewer to editor) must update
	// in place, not create a duplicate row for the same (dashboard, user).
	if err := s.SetDashboardPermission(ctx, dashboardID, grantee.ID, RoleViewer, creator.ID); err != nil {
		t.Fatalf("SetDashboardPermission (update): %v", err)
	}
	got, err = s.GetDashboardPermission(ctx, dashboardID, grantee.ID)
	if err != nil {
		t.Fatalf("GetDashboardPermission after update: %v", err)
	}
	if got.Role != RoleViewer {
		t.Fatalf("role after update = %q, want viewer", got.Role)
	}
}

func TestSetDashboardPermissionRequiresGrantedBy(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	user, err := s.UpsertUserBySSO(ctx, "sub-"+uniqueSuffix(), "user-"+uniqueSuffix()+"@example.com", "User")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, user.ID)

	if err := s.SetDashboardPermission(ctx, dashboardID, user.ID, RoleEditor, ""); err == nil {
		t.Fatal("expected an error for an empty grantedBy -- every grant must be attributable")
	}
}

func TestGetDashboardPermissionNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetDashboardPermission(context.Background(), uuid.NewString(), uuid.NewString()); err != ErrNotFound {
		t.Fatalf("GetDashboardPermission error = %v, want ErrNotFound", err)
	}
}

func TestRevokeDashboardPermission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	creator, err := s.UpsertUserBySSO(ctx, "sub-creator-"+uniqueSuffix(), "creator-"+uniqueSuffix()+"@example.com", "Creator")
	if err != nil {
		t.Fatalf("UpsertUserBySSO creator: %v", err)
	}
	grantee, err := s.UpsertUserBySSO(ctx, "sub-grantee-"+uniqueSuffix(), "grantee-"+uniqueSuffix()+"@example.com", "Grantee")
	if err != nil {
		t.Fatalf("UpsertUserBySSO grantee: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, creator.ID)
	if err := s.SetDashboardPermission(ctx, dashboardID, grantee.ID, RoleEditor, creator.ID); err != nil {
		t.Fatalf("SetDashboardPermission: %v", err)
	}

	if err := s.RevokeDashboardPermission(ctx, dashboardID, grantee.ID); err != nil {
		t.Fatalf("RevokeDashboardPermission: %v", err)
	}
	if _, err := s.GetDashboardPermission(ctx, dashboardID, grantee.ID); err != ErrNotFound {
		t.Fatalf("GetDashboardPermission after revoke = %v, want ErrNotFound", err)
	}
}

func TestListDashboardPermissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	creator, err := s.UpsertUserBySSO(ctx, "sub-creator-"+uniqueSuffix(), "creator-"+uniqueSuffix()+"@example.com", "Creator")
	if err != nil {
		t.Fatalf("UpsertUserBySSO creator: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, creator.ID)
	otherDashboardID := createTestDashboard(t, s, tenantID, creator.ID)

	for i := 0; i < 2; i++ {
		grantee, err := s.UpsertUserBySSO(ctx, fmt.Sprintf("sub-grantee-%d-%s", i, uniqueSuffix()), fmt.Sprintf("grantee-%d-%s@example.com", i, uniqueSuffix()), "Grantee")
		if err != nil {
			t.Fatalf("UpsertUserBySSO grantee %d: %v", i, err)
		}
		if err := s.SetDashboardPermission(ctx, dashboardID, grantee.ID, RoleEditor, creator.ID); err != nil {
			t.Fatalf("SetDashboardPermission %d: %v", i, err)
		}
	}
	// A grant on a different dashboard must not leak into this one's list.
	otherGrantee, err := s.UpsertUserBySSO(ctx, "sub-other-"+uniqueSuffix(), "other-"+uniqueSuffix()+"@example.com", "Other")
	if err != nil {
		t.Fatalf("UpsertUserBySSO otherGrantee: %v", err)
	}
	if err := s.SetDashboardPermission(ctx, otherDashboardID, otherGrantee.ID, RoleViewer, creator.ID); err != nil {
		t.Fatalf("SetDashboardPermission otherDashboard: %v", err)
	}

	list, err := s.ListDashboardPermissions(ctx, dashboardID)
	if err != nil {
		t.Fatalf("ListDashboardPermissions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
}

// TestDashboardPermissionsAdapterImplementsPermissionStore drives the
// adapter (dashboards_adapter.go) end to end -- the same interface
// api/dashboards.Handler actually calls -- rather than only testing the
// raw Store methods above, so a mismatch between the two (e.g. a bad
// authz.Role<->Role conversion) would be caught here.
func TestDashboardPermissionsAdapterImplementsPermissionStore(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	creator, err := s.UpsertUserBySSO(ctx, "sub-creator-"+uniqueSuffix(), "creator-"+uniqueSuffix()+"@example.com", "Creator")
	if err != nil {
		t.Fatalf("UpsertUserBySSO creator: %v", err)
	}
	grantee, err := s.UpsertUserBySSO(ctx, "sub-grantee-"+uniqueSuffix(), "grantee-"+uniqueSuffix()+"@example.com", "Grantee")
	if err != nil {
		t.Fatalf("UpsertUserBySSO grantee: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, creator.ID)

	adapter := NewDashboardPermissions(s)

	if _, ok, err := adapter.GrantedRole(ctx, dashboardID, grantee.ID); err != nil || ok {
		t.Fatalf("GrantedRole before any grant = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	if err := adapter.SetPermission(ctx, dashboardID, grantee.ID, authz.RoleEditor, creator.ID); err != nil {
		t.Fatalf("SetPermission: %v", err)
	}
	role, ok, err := adapter.GrantedRole(ctx, dashboardID, grantee.ID)
	if err != nil || !ok || role != authz.RoleEditor {
		t.Fatalf("GrantedRole = (%v, %v, %v), want (editor, true, nil)", role, ok, err)
	}

	list, err := adapter.ListPermissions(ctx, dashboardID)
	if err != nil || len(list) != 1 || list[0].Role != authz.RoleEditor {
		t.Fatalf("ListPermissions = (%+v, %v), want one editor grant", list, err)
	}

	if err := adapter.RevokePermission(ctx, dashboardID, grantee.ID); err != nil {
		t.Fatalf("RevokePermission: %v", err)
	}
	if _, ok, _ := adapter.GrantedRole(ctx, dashboardID, grantee.ID); ok {
		t.Fatal("expected the grant to be revoked")
	}
}

// TestDashboardPermissionsAdapterRejectsAdminRole is the regression test
// for Permission's doc comment: Admin/Owner already have tenant-wide
// dashboard access, so a resource-level grant of "admin" is meaningless
// under this design and metadata/migrations/0033 tightened the CHECK
// constraint to match -- the adapter must reject it before it ever
// reaches SQL, not rely on the constraint alone.
func TestDashboardPermissionsAdapterRejectsAdminRole(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	creator, err := s.UpsertUserBySSO(ctx, "sub-creator-"+uniqueSuffix(), "creator-"+uniqueSuffix()+"@example.com", "Creator")
	if err != nil {
		t.Fatalf("UpsertUserBySSO creator: %v", err)
	}
	dashboardID := createTestDashboard(t, s, tenantID, creator.ID)

	adapter := NewDashboardPermissions(s)
	if err := adapter.SetPermission(ctx, dashboardID, uuid.NewString(), authz.RoleAdmin, creator.ID); err == nil {
		t.Fatal("expected an error granting role=admin via a dashboard permission")
	}
}
