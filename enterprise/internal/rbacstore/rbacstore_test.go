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
