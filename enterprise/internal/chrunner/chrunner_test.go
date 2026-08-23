// Integration tests against a real ClickHouse -- exercises the actual
// question this package exists to answer: does a query authenticated as
// tenant A ever see tenant B's data. Uses enterprise/internal/
// tenantprovision to set up real per-tenant users first (this is the
// adversarial probe api/queryapi/tenant_isolation_gap_test.go's
// TestAdversarial_ClickHouseUserCannotReadOtherTenantDatabaseByFullyQualifiedName
// names as blocked -- this is where it stops being blocked, at the
// chrunner/query-execution layer specifically, complementing
// tenantprovision's own version of the same probe at the raw-SQL-user
// layer).
//
// Skipped unless CHRUNNER_TEST_CLICKHOUSE_ADDR is set; run via:
//
//	docker run --rm --network cairnobs_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e CHRUNNER_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
//	  -e CHRUNNER_TEST_CLICKHOUSE_PASSWORD=cairnobs-dev-only \
//	  golang:1.25-alpine go test ./internal/chrunner/... -v
package chrunner

import (
	"context"
	"fmt"
	"os"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/cairnobs/cairnobs/api/authz"

	"github.com/cairnobs/cairnobs/enterprise/internal/tenantprovision"
)

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("CHRUNNER_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("CHRUNNER_TEST_CLICKHOUSE_ADDR not set -- skipping live-ClickHouse integration test")
	}
	return addr
}

func provisionTestTenant(t *testing.T, addr string) (tenantID string, creds tenantprovision.Credentials) {
	t.Helper()
	admin, err := chdriver.Open(&chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{Database: "default", Username: "default", Password: os.Getenv("CHRUNNER_TEST_CLICKHOUSE_PASSWORD")},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	tenantID = "cr" + uuid.NewString()[:8]
	creds, err = tenantprovision.New(admin).ProvisionClickHouse(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("provisioning tenant %s: %v", tenantID, err)
	}
	return tenantID, creds
}

func TestRegistryRoutesQueryToCorrectTenant(t *testing.T) {
	addr := testAddr(t)
	ctx := context.Background()
	tenantA, credsA := provisionTestTenant(t, addr)

	// Seed a distinguishing row directly as the tenant (SELECT-only
	// grant means chrunner's own connection can't INSERT -- use a
	// throwaway admin connection to seed data, matching how a real
	// deployment's ingest path would write, not how api's read-only
	// query path does).
	admin, err := chdriver.Open(&chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{Database: "default", Username: "default", Password: os.Getenv("CHRUNNER_TEST_CLICKHOUSE_PASSWORD")},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE TABLE `%s`.marker (id UInt8) ENGINE = Memory", tenantA)); err != nil {
		t.Fatalf("creating marker table: %v", err)
	}
	if err := admin.Exec(ctx, fmt.Sprintf("INSERT INTO `%s`.marker VALUES (42)", tenantA)); err != nil {
		t.Fatalf("seeding marker row: %v", err)
	}

	reg, err := New(ctx, addr, []DataSource{
		{TenantID: tenantA, Database: tenantA, Username: credsA.Username, Password: credsA.Password},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	reqCtx := authz.WithIdentity(ctx, authz.Identity{TenantID: tenantA, Role: authz.RoleViewer})
	result, err := reg.RunSQL(reqCtx, "SELECT id FROM marker")
	if err != nil {
		t.Fatalf("RunSQL: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != uint8(42) {
		t.Fatalf("unexpected result: %+v", result.Rows)
	}
}

func TestRegistryRefusesQueryWithNoTenantContext(t *testing.T) {
	addr := testAddr(t)
	ctx := context.Background()
	tenantA, credsA := provisionTestTenant(t, addr)

	reg, err := New(ctx, addr, []DataSource{
		{TenantID: tenantA, Database: tenantA, Username: credsA.Username, Password: credsA.Password},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	if _, err := reg.RunSQL(ctx, "SELECT 1"); err == nil {
		t.Fatal("expected RunSQL to refuse a request with no authenticated identity in context")
	}
}

func TestRegistryRefusesUnknownTenant(t *testing.T) {
	addr := testAddr(t)
	ctx := context.Background()
	tenantA, credsA := provisionTestTenant(t, addr)

	reg, err := New(ctx, addr, []DataSource{
		{TenantID: tenantA, Database: tenantA, Username: credsA.Username, Password: credsA.Password},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	reqCtx := authz.WithIdentity(ctx, authz.Identity{TenantID: "some-other-tenant-never-provisioned", Role: authz.RoleViewer})
	if _, err := reg.RunSQL(reqCtx, "SELECT 1"); err == nil {
		t.Fatal("expected RunSQL to refuse a tenant with no provisioned connection, not silently fall back")
	}
}

// TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL is the full
// end-to-end adversarial probe: two tenants, two connections inside one
// Registry, and a raw-SQL attempt (which the query language's escape
// hatch would pass straight through unmodified) to read the other
// tenant's data by fully-qualified name. This is what proves the
// connection-layer isolation model actually holds through chrunner, not
// just through tenantprovision's own grants (already covered by
// tenantprovision_test.go) -- this test exercises the exact code path
// api/queryapi.Handler calls in production.
func TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL(t *testing.T) {
	addr := testAddr(t)
	ctx := context.Background()
	tenantA, credsA := provisionTestTenant(t, addr)
	tenantB, credsB := provisionTestTenant(t, addr)

	admin, err := chdriver.Open(&chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{Database: "default", Username: "default", Password: os.Getenv("CHRUNNER_TEST_CLICKHOUSE_PASSWORD")},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	defer admin.Close()
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE TABLE `%s`.secret (id UInt8) ENGINE = Memory", tenantB)); err != nil {
		t.Fatalf("creating secret table: %v", err)
	}
	if err := admin.Exec(ctx, fmt.Sprintf("INSERT INTO `%s`.secret VALUES (99)", tenantB)); err != nil {
		t.Fatalf("seeding secret row: %v", err)
	}

	reg, err := New(ctx, addr, []DataSource{
		{TenantID: tenantA, Database: tenantA, Username: credsA.Username, Password: credsA.Password},
		{TenantID: tenantB, Database: tenantB, Username: credsB.Username, Password: credsB.Password},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	reqCtx := authz.WithIdentity(ctx, authz.Identity{TenantID: tenantA, Role: authz.RoleViewer})
	_, err = reg.RunSQL(reqCtx, fmt.Sprintf("SELECT * FROM `%s`.secret", tenantB))
	if err == nil {
		t.Fatal("tenant A's request was able to read tenant B's database by fully-qualified name -- isolation is broken")
	}
}

// TestRegistryRefusesMidProvisioningTenant is Phase 4 task 8's item 4
// adversarial probe (see /docs/phase-4-isolation-design.md's
// verification plan and api/queryapi/tenant_isolation_gap_test.go):
// a tenant row that exists in rbacstore but hasn't reached the
// active+credentialed gate yet must be refused, not served via some
// ambient connection. Unlike every other test in this file, this one
// needs no live ClickHouse at all -- New never dials out for an empty
// DataSource list, so an empty Registry (as if every tenant in
// `tenants` were still mid-provisioning) is exactly what
// enterprise-api's main.go would build from
// rbacstore.ListProvisionedDataSources before any tenant clears that
// filter. "Mid-provisioning" and "entirely unknown" collapse to the
// identical code path here by construction: Registry has no concept of
// "a tenant row exists," only of "a runner is in my map" -- the real
// gate is ListProvisionedDataSources's SQL WHERE clause, already
// covered by rbacstore_test.go's
// TestListProvisionedDataSourcesExcludesUnprovisionedAndInactive. This
// test is the Docker-free proof that RunSQL's refusal actually holds on
// the empty-map end of that gate, complementing
// TestRegistryRefusesUnknownTenant's live-ClickHouse proof on the
// populated end.
func TestRegistryRefusesMidProvisioningTenant(t *testing.T) {
	ctx := context.Background()
	reg, err := New(ctx, "unused:9000", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	reqCtx := authz.WithIdentity(ctx, authz.Identity{TenantID: "mid-provisioning-tenant", Role: authz.RoleViewer})
	if _, err := reg.RunSQL(reqCtx, "SELECT 1"); err == nil {
		t.Fatal("expected RunSQL to refuse a tenant that hasn't reached the active+credentialed gate, not silently serve it")
	}
}
