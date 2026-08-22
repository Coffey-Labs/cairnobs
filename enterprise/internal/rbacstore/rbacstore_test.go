// Integration tests against a real Postgres -- rbacstore's whole job is
// SQL (upserts, FK constraints, unique constraints on
// (tenant_id, user_id)/(dashboard_id, user_id)), so a mocked pool
// wouldn't actually exercise it. Skipped unless RBACSTORE_TEST_POSTGRES_ADDR
// is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e RBACSTORE_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e RBACSTORE_TEST_POSTGRES_PASSWORD=cairnobs-dev-only \
//	  golang:1.25-alpine go test ./internal/rbacstore/... -v
package rbacstore

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cairnobs/cairnobs/api/authz"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("RBACSTORE_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("RBACSTORE_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("RBACSTORE_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://cairnobs:%s@%s/cairnobs_metadata", password, addr)
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

func TestTransferOwnerMovesOwnershipAndDowngradesPreviousOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	oldOwner, err := s.UpsertUserBySSO(ctx, "sub-old-owner-"+uniqueSuffix(), "old-owner-"+uniqueSuffix()+"@example.com", "Old Owner")
	if err != nil {
		t.Fatalf("UpsertUserBySSO (old owner): %v", err)
	}
	newOwner, err := s.UpsertUserBySSO(ctx, "sub-new-owner-"+uniqueSuffix(), "new-owner-"+uniqueSuffix()+"@example.com", "New Owner")
	if err != nil {
		t.Fatalf("UpsertUserBySSO (new owner): %v", err)
	}
	if err := s.SetMembership(ctx, tenantID, oldOwner.ID, RoleOwner); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	if err := s.SetOwner(ctx, tenantID, oldOwner.ID); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	if err := s.TransferOwner(ctx, tenantID, newOwner.ID, RoleAdmin); err != nil {
		t.Fatalf("TransferOwner: %v", err)
	}

	tenant, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if tenant.OwnerUserID != newOwner.ID {
		t.Fatalf("tenant OwnerUserID = %q, want %q", tenant.OwnerUserID, newOwner.ID)
	}

	newOwnerMembership, err := s.GetMembership(ctx, tenantID, newOwner.ID)
	if err != nil {
		t.Fatalf("GetMembership (new owner): %v", err)
	}
	if newOwnerMembership.Role != RoleOwner {
		t.Fatalf("new owner's role = %q, want owner", newOwnerMembership.Role)
	}

	// The point of TransferOwner over calling SetOwner alone: the
	// previous owner keeps a real membership (downgraded, not deleted or
	// left stale at "owner") -- otherwise tenant_memberships would claim
	// two owners while tenants.owner_user_id can only name one.
	oldOwnerMembership, err := s.GetMembership(ctx, tenantID, oldOwner.ID)
	if err != nil {
		t.Fatalf("GetMembership (old owner): %v", err)
	}
	if oldOwnerMembership.Role != RoleAdmin {
		t.Fatalf("old owner's role after transfer = %q, want admin (downgraded, not left as owner)", oldOwnerMembership.Role)
	}

	// The previous owner is no longer Owner, so RevokeMembership must now
	// accept revoking them -- proves the downgrade is real, not cosmetic.
	if err := s.RevokeMembership(ctx, tenantID, oldOwner.ID); err != nil {
		t.Fatalf("RevokeMembership on the downgraded former owner: %v", err)
	}
}

func TestTransferOwnerRefusesTenantWithNoCurrentOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	newOwner, err := s.UpsertUserBySSO(ctx, "sub-"+uniqueSuffix(), "user-"+uniqueSuffix()+"@example.com", "Someone")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}

	if err := s.TransferOwner(ctx, tenantID, newOwner.ID, RoleAdmin); err == nil {
		t.Fatal("expected TransferOwner to refuse a tenant with no current owner")
	}
}

func TestTransferOwnerRefusesTransferToCurrentOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	owner, err := s.UpsertUserBySSO(ctx, "sub-"+uniqueSuffix(), "owner-"+uniqueSuffix()+"@example.com", "Owner")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}
	if err := s.SetMembership(ctx, tenantID, owner.ID, RoleOwner); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	if err := s.SetOwner(ctx, tenantID, owner.ID); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	if err := s.TransferOwner(ctx, tenantID, owner.ID, RoleAdmin); err == nil {
		t.Fatal("expected TransferOwner to refuse transferring ownership to its current holder")
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

	ds, err := s.CreateDataSource(ctx, tenantID, "default", tenantID, "/var/lib/cairnobs-search/tenants/"+tenantID)
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

func TestListActiveTenantIDsExcludesNonActive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	active := "test-tenant-" + uniqueSuffix()
	provisioning := "test-tenant-" + uniqueSuffix()

	for _, id := range []string{active, provisioning} {
		if _, err := s.CreateTenant(ctx, id, id); err != nil {
			t.Fatalf("CreateTenant %s: %v", id, err)
		}
	}
	if err := s.SetTenantStatus(ctx, active, "active"); err != nil {
		t.Fatalf("SetTenantStatus active: %v", err)
	}
	// provisioning stays in 'provisioning' (CreateTenant's default).

	ids, err := s.ListActiveTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListActiveTenantIDs: %v", err)
	}
	foundActive := false
	for _, id := range ids {
		if id == provisioning {
			t.Fatalf("non-active tenant %q leaked into ListActiveTenantIDs", provisioning)
		}
		if id == active {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatal("expected the active tenant to be in the list")
	}
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

// TestGetUserByEmail is the regression test for
// cmd/enterprise-auth/main.go's -grant-membership-user-email flag,
// which looks up an existing user by email rather than requiring the
// operator to already know their generated UUID.
func TestGetUserByEmail(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	email := "lookup-" + uniqueSuffix() + "@example.com"

	created, err := s.UpsertUserBySSO(ctx, "sub-"+uniqueSuffix(), email, "Lookup Me")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}

	got, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("GetUserByEmail returned ID %q, want %q", got.ID, created.ID)
	}
}

func TestGetUserByEmailNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetUserByEmail(context.Background(), "does-not-exist-"+uniqueSuffix()+"@example.com"); err != ErrNotFound {
		t.Fatalf("GetUserByEmail error = %v, want ErrNotFound", err)
	}
}

// TestTenantIsActive is the rbacstore-side half of Phase 4 task 8's
// item 4 adversarial probe -- enterprise/internal/searchclient's
// TestSearchRefusesMidProvisioningTenant proves Search refuses when
// TenantChecker.TenantIsActive returns false; this proves the real
// implementation actually returns false for a mid-provisioning tenant
// and only ever returns true once SetTenantStatus marks it active.
func TestTenantIsActive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()

	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	active, err := s.TenantIsActive(ctx, tenantID)
	if err != nil {
		t.Fatalf("TenantIsActive (provisioning): %v", err)
	}
	if active {
		t.Fatal("a freshly-created tenant (status 'provisioning') must not be active")
	}

	if err := s.SetTenantStatus(ctx, tenantID, "active"); err != nil {
		t.Fatalf("SetTenantStatus: %v", err)
	}
	active, err = s.TenantIsActive(ctx, tenantID)
	if err != nil {
		t.Fatalf("TenantIsActive (active): %v", err)
	}
	if !active {
		t.Fatal("expected the tenant to be active after SetTenantStatus")
	}
}

func TestTenantIsActiveNonexistentTenant(t *testing.T) {
	s := testStore(t)
	active, err := s.TenantIsActive(context.Background(), "does-not-exist-"+uniqueSuffix())
	if err != nil {
		t.Fatalf("TenantIsActive: %v, want a plain (false, nil) for a nonexistent tenant, not an error", err)
	}
	if active {
		t.Fatal("a nonexistent tenant must not be reported active")
	}
}

func TestRevokeMembership(t *testing.T) {
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
	if err := s.SetMembership(ctx, tenantID, user.ID, RoleEditor); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	if err := s.RevokeMembership(ctx, tenantID, user.ID); err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if _, err := s.GetMembership(ctx, tenantID, user.ID); err != ErrNotFound {
		t.Fatalf("GetMembership after revoke = %v, want ErrNotFound", err)
	}
}

func TestRevokeMembershipNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.RevokeMembership(ctx, tenantID, uuid.NewString()); err != ErrNotFound {
		t.Fatalf("RevokeMembership error = %v, want ErrNotFound", err)
	}
}

// TestRevokeMembershipRefusesCurrentOwner is the regression test for
// RevokeMembership's doc comment: deleting the Owner's membership
// without transferring ownership first would leave tenants.owner_user_id
// pointing at a user with no membership in the tenant at all.
func TestRevokeMembershipRefusesCurrentOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	owner, err := s.UpsertUserBySSO(ctx, "sub-owner-"+uniqueSuffix(), "owner-"+uniqueSuffix()+"@example.com", "Owner")
	if err != nil {
		t.Fatalf("UpsertUserBySSO: %v", err)
	}
	if err := s.SetMembership(ctx, tenantID, owner.ID, RoleOwner); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	if err := s.SetOwner(ctx, tenantID, owner.ID); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	if err := s.RevokeMembership(ctx, tenantID, owner.ID); err == nil {
		t.Fatal("expected RevokeMembership to refuse revoking the tenant's current Owner")
	}
	if _, err := s.GetMembership(ctx, tenantID, owner.ID); err != nil {
		t.Fatalf("owner's membership must still exist after the refused revoke, GetMembership: %v", err)
	}
}

func TestListMembershipsForTenant(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	otherTenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.CreateTenant(ctx, otherTenantID, "Other Tenant"); err != nil {
		t.Fatalf("CreateTenant other: %v", err)
	}

	viewer, err := s.UpsertUserBySSO(ctx, "sub-viewer-"+uniqueSuffix(), "viewer-"+uniqueSuffix()+"@example.com", "Viewer")
	if err != nil {
		t.Fatalf("UpsertUserBySSO viewer: %v", err)
	}
	if err := s.SetMembership(ctx, tenantID, viewer.ID, RoleViewer); err != nil {
		t.Fatalf("SetMembership viewer: %v", err)
	}
	editor, err := s.UpsertUserBySSO(ctx, "sub-editor-"+uniqueSuffix(), "editor-"+uniqueSuffix()+"@example.com", "Editor")
	if err != nil {
		t.Fatalf("UpsertUserBySSO editor: %v", err)
	}
	if err := s.SetMembership(ctx, tenantID, editor.ID, RoleEditor); err != nil {
		t.Fatalf("SetMembership editor: %v", err)
	}
	// A membership in a different tenant must not leak into this list.
	elsewhere, err := s.UpsertUserBySSO(ctx, "sub-elsewhere-"+uniqueSuffix(), "elsewhere-"+uniqueSuffix()+"@example.com", "Elsewhere")
	if err != nil {
		t.Fatalf("UpsertUserBySSO elsewhere: %v", err)
	}
	if err := s.SetMembership(ctx, otherTenantID, elsewhere.ID, RoleAdmin); err != nil {
		t.Fatalf("SetMembership elsewhere: %v", err)
	}

	members, err := s.ListMembershipsForTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListMembershipsForTenant: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("len(members) = %d, want 2", len(members))
	}
	byEmail := map[string]TenantMember{}
	for _, m := range members {
		byEmail[m.Email] = m
	}
	if got := byEmail[viewer.Email]; got.Role != RoleViewer || got.UserID != viewer.ID {
		t.Fatalf("unexpected viewer entry: %+v", got)
	}
	if got := byEmail[editor.Email]; got.Role != RoleEditor || got.UserID != editor.ID {
		t.Fatalf("unexpected editor entry: %+v", got)
	}
}

func TestCreateAndValidateIngestCredential(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	token, err := s.CreateIngestCredential(ctx, tenantID)
	if err != nil {
		t.Fatalf("CreateIngestCredential: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}

	got, err := s.ValidateIngestCredential(ctx, token)
	if err != nil {
		t.Fatalf("ValidateIngestCredential: %v", err)
	}
	if got != tenantID {
		t.Fatalf("ValidateIngestCredential tenant = %q, want %q", got, tenantID)
	}
}

func TestValidateIngestCredentialRejectsUnknownToken(t *testing.T) {
	s := testStore(t)
	if _, err := s.ValidateIngestCredential(context.Background(), "not-a-real-token"); err != ErrNotFound {
		t.Fatalf("ValidateIngestCredential error = %v, want ErrNotFound", err)
	}
}

// TestIngestCredentialTokenNeverStoredAsPlaintext is the regression test
// for this table's whole reason for hashing: the raw token string must
// not appear anywhere in the persisted row (only its hash), so a
// database leak doesn't hand out usable credentials.
func TestIngestCredentialTokenNeverStoredAsPlaintext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	token, err := s.CreateIngestCredential(ctx, tenantID)
	if err != nil {
		t.Fatalf("CreateIngestCredential: %v", err)
	}

	var stored string
	row := s.pool.QueryRow(ctx, `SELECT token_hash FROM ingest_credentials WHERE tenant_id = $1`, tenantID)
	if err := row.Scan(&stored); err != nil {
		t.Fatalf("reading stored token_hash: %v", err)
	}
	if stored == token {
		t.Fatal("the plaintext token must never be stored directly in token_hash")
	}
	if stored != hashIngestToken(token) {
		t.Fatalf("stored hash = %q, want sha256(token) = %q", stored, hashIngestToken(token))
	}
}

func TestRevokeIngestCredential(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	token, err := s.CreateIngestCredential(ctx, tenantID)
	if err != nil {
		t.Fatalf("CreateIngestCredential: %v", err)
	}
	creds, err := s.ListIngestCredentialsForTenant(ctx, tenantID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("ListIngestCredentialsForTenant = (%+v, %v), want exactly one", creds, err)
	}

	if err := s.RevokeIngestCredential(ctx, creds[0].ID); err != nil {
		t.Fatalf("RevokeIngestCredential: %v", err)
	}
	if _, err := s.ValidateIngestCredential(ctx, token); err != ErrNotFound {
		t.Fatalf("ValidateIngestCredential after revoke = %v, want ErrNotFound", err)
	}
}

func TestRevokeIngestCredentialNotFound(t *testing.T) {
	s := testStore(t)
	if err := s.RevokeIngestCredential(context.Background(), uuid.NewString()); err != ErrNotFound {
		t.Fatalf("RevokeIngestCredential error = %v, want ErrNotFound", err)
	}
}

func TestListIngestCredentialsForTenantExcludesOtherTenants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tenantID := "test-tenant-" + uniqueSuffix()
	otherTenantID := "test-tenant-" + uniqueSuffix()
	if _, err := s.CreateTenant(ctx, tenantID, "Test Tenant"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.CreateTenant(ctx, otherTenantID, "Other Tenant"); err != nil {
		t.Fatalf("CreateTenant other: %v", err)
	}
	if _, err := s.CreateIngestCredential(ctx, tenantID); err != nil {
		t.Fatalf("CreateIngestCredential: %v", err)
	}
	if _, err := s.CreateIngestCredential(ctx, otherTenantID); err != nil {
		t.Fatalf("CreateIngestCredential other: %v", err)
	}

	creds, err := s.ListIngestCredentialsForTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListIngestCredentialsForTenant: %v", err)
	}
	if len(creds) != 1 || creds[0].TenantID != tenantID {
		t.Fatalf("unexpected credentials: %+v", creds)
	}
}
