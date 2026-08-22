// Adversarial cross-tenant isolation tests against a real Postgres --
// Phase 4 task 8. handler_test.go's TestCrossTenant* tests already cover
// this against fakeStore (which is hand-written to mimic the real SQL's
// tenant filtering); these tests exercise the actual parameterized SQL
// in store.go, including the tenant_id foreign key constraint added in
// metadata/migrations/0027_add_dashboards_tenant_fk.sql -- a real gap a
// fake store literally cannot catch (e.g. a typo in a WHERE clause, or
// forgetting to update every method when the schema changes).
//
// Skipped unless DASHBOARDS_TEST_POSTGRES_ADDR is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/api \
//	  -e DASHBOARDS_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e DASHBOARDS_TEST_POSTGRES_PASSWORD=cairnobs-dev-only \
//	  golang:1.25-alpine go test ./dashboards/... -run Integration -v
package dashboards

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	addr := os.Getenv("DASHBOARDS_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("DASHBOARDS_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("DASHBOARDS_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://cairnobs:%s@%s/cairnobs_metadata", password, addr)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool), pool
}

// createTestTenant inserts directly into the tenants table (owned by
// metadata/migrations/0017-0019, not this package) -- dashboards.tenant_id
// has had a real foreign-key constraint since
// metadata/migrations/0027_add_dashboards_tenant_fk.sql, so a dashboard
// row for a tenant that doesn't exist in `tenants` is rejected by
// Postgres itself, not just application logic. Uses a unique suffix so
// repeated test runs against a persistent dev Postgres don't collide.
func createTestTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "test-tenant-" + uuid.NewString()[:8]
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tenants (id, display_name, status) VALUES ($1, $1, 'active')`, id)
	if err != nil {
		t.Fatalf("creating test tenant: %v", err)
	}
	return id
}

func TestIntegrationCrossTenantGetIsNotFound(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	d := &Dashboard{TenantID: tenantA, Name: "Acme's dashboard"}
	if err := store.CreateDashboard(ctx, d); err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// Same tenant: found.
	if _, err := store.GetDashboard(ctx, tenantA, d.ID); err != nil {
		t.Fatalf("GetDashboard (same tenant): %v", err)
	}
	// Different tenant: not found, not a data leak.
	if _, err := store.GetDashboard(ctx, tenantB, d.ID); err != ErrNotFound {
		t.Fatalf("GetDashboard (cross-tenant) error = %v, want ErrNotFound", err)
	}
}

func TestIntegrationCrossTenantListDoesNotLeak(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	da := &Dashboard{TenantID: tenantA, Name: "A's dashboard"}
	db := &Dashboard{TenantID: tenantB, Name: "B's dashboard"}
	if err := store.CreateDashboard(ctx, da); err != nil {
		t.Fatalf("CreateDashboard A: %v", err)
	}
	if err := store.CreateDashboard(ctx, db); err != nil {
		t.Fatalf("CreateDashboard B: %v", err)
	}

	listA, err := store.ListDashboards(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListDashboards A: %v", err)
	}
	for _, d := range listA {
		if d.TenantID != tenantA {
			t.Fatalf("tenant A's list leaked a dashboard from tenant %q", d.TenantID)
		}
	}
	found := false
	for _, d := range listA {
		if d.ID == da.ID {
			found = true
		}
		if d.ID == db.ID {
			t.Fatalf("tenant A's list included tenant B's dashboard %q", db.ID)
		}
	}
	if !found {
		t.Fatal("tenant A's list did not include tenant A's own dashboard")
	}
}

func TestIntegrationCrossTenantUpdateAndDeleteAreNotFound(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	d := &Dashboard{TenantID: tenantA, Name: "Original"}
	if err := store.CreateDashboard(ctx, d); err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	hijack := &Dashboard{ID: d.ID, Name: "Hijacked"}
	if err := store.UpdateDashboard(ctx, tenantB, hijack); err != ErrNotFound {
		t.Fatalf("cross-tenant UpdateDashboard error = %v, want ErrNotFound", err)
	}
	got, err := store.GetDashboard(ctx, tenantA, d.ID)
	if err != nil {
		t.Fatalf("GetDashboard after attempted cross-tenant update: %v", err)
	}
	if got.Name != "Original" {
		t.Fatalf("cross-tenant update mutated the row: Name = %q", got.Name)
	}

	if err := store.DeleteDashboard(ctx, tenantB, d.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant DeleteDashboard error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetDashboard(ctx, tenantA, d.ID); err != nil {
		t.Fatalf("expected the dashboard to still exist after a failed cross-tenant delete: %v", err)
	}
}

func TestIntegrationCrossTenantPanelMutationIsNotFound(t *testing.T) {
	store, pool := integrationStore(t)
	ctx := context.Background()
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	d := &Dashboard{TenantID: tenantA, Name: "Has panels"}
	if err := store.CreateDashboard(ctx, d); err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	p := &Panel{Query: "service=api", VizType: VizTable, Width: 6, Height: 4}
	if err := store.AddPanel(ctx, tenantB, d.ID, p); err != ErrNotFound {
		t.Fatalf("cross-tenant AddPanel error = %v, want ErrNotFound", err)
	}

	// Add it for real (tenant A), then confirm tenant B can't update/delete it either.
	if err := store.AddPanel(ctx, tenantA, d.ID, p); err != nil {
		t.Fatalf("AddPanel (same tenant): %v", err)
	}
	p.Title = "Hijacked"
	if err := store.UpdatePanel(ctx, tenantB, p); err != ErrNotFound {
		t.Fatalf("cross-tenant UpdatePanel error = %v, want ErrNotFound", err)
	}
	if err := store.DeletePanel(ctx, tenantB, d.ID, p.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant DeletePanel error = %v, want ErrNotFound", err)
	}
}

// TestIntegrationDashboardTenantForeignKeyRejectsUnknownTenant proves
// the database itself, not just application code, refuses a dashboard
// for a tenant that was never provisioned -- defense in depth
// independent of the Go-level tenant scoping above (see
// metadata/migrations/0027_add_dashboards_tenant_fk.sql).
func TestIntegrationDashboardTenantForeignKeyRejectsUnknownTenant(t *testing.T) {
	store, _ := integrationStore(t)
	d := &Dashboard{TenantID: "does-not-exist-" + uuid.NewString()[:8], Name: "Orphan"}
	if err := store.CreateDashboard(context.Background(), d); err == nil {
		t.Fatal("expected CreateDashboard to fail for a tenant_id with no matching tenants row")
	}
}
