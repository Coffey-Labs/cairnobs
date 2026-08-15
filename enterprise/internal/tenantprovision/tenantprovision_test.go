// Integration tests against a real ClickHouse -- this package's whole
// job is DDL side effects (CREATE DATABASE/USER, GRANT), which a mock
// driver.Conn can't meaningfully verify. Skipped unless
// TENANTPROVISION_TEST_CLICKHOUSE_ADDR is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e TENANTPROVISION_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
//	  -e TENANTPROVISION_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
//	  golang:1.25-alpine go test ./internal/tenantprovision/... -v
package tenantprovision

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

func testAdminConn(t *testing.T) driver.Conn {
	t.Helper()
	addr := os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("TENANTPROVISION_TEST_CLICKHOUSE_ADDR not set -- skipping live-ClickHouse integration test")
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: "default",
			Password: os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_PASSWORD"),
		},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("pinging clickhouse: %v", err)
	}
	return conn
}

func testTenantID() string {
	return "tp" + uuid.NewString()[:8]
}

func TestProvisionClickHouseCreatesUsableTenantConnection(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	tenantID := testTenantID()
	ctx := context.Background()

	creds, err := p.ProvisionClickHouse(ctx, tenantID)
	if err != nil {
		t.Fatalf("ProvisionClickHouse: %v", err)
	}
	if creds.Username != "tenant_"+tenantID || creds.Password == "" {
		t.Fatalf("unexpected credentials: %+v", creds)
	}

	// Prove the credential actually works: connect as the tenant user
	// and run a real query against its own database.
	tenantConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_ADDR")},
		Auth: clickhouse.Auth{Database: tenantID, Username: creds.Username, Password: creds.Password},
	})
	if err != nil {
		t.Fatalf("opening tenant connection: %v", err)
	}
	defer tenantConn.Close()
	if err := tenantConn.Ping(ctx); err != nil {
		t.Fatalf("pinging as the provisioned tenant user: %v", err)
	}
	if err := tenantConn.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("running SELECT as the provisioned tenant user: %v", err)
	}
}

// TestProvisionedUserCanInsertIntoOwnDatabase is the regression test for
// a real bug found while building enterprise/internal/chwriter: this
// credential is also the one chwriter.Registry uses to write ingested
// records, so it must be able to INSERT into its own database, not just
// SELECT from it -- the grant originally only covered SELECT, which
// would have made every real per-tenant ClickHouse write fail with a
// permission error.
func TestProvisionedUserCanInsertIntoOwnDatabase(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	tenantID := testTenantID()
	ctx := context.Background()

	creds, err := p.ProvisionClickHouse(ctx, tenantID)
	if err != nil {
		t.Fatalf("ProvisionClickHouse: %v", err)
	}
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE TABLE `%s`.marker (id UInt8) ENGINE = Memory", tenantID)); err != nil {
		t.Fatalf("creating marker table: %v", err)
	}

	tenantConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_ADDR")},
		Auth: clickhouse.Auth{Database: tenantID, Username: creds.Username, Password: creds.Password},
	})
	if err != nil {
		t.Fatalf("opening tenant connection: %v", err)
	}
	defer tenantConn.Close()

	if err := tenantConn.Exec(ctx, "INSERT INTO marker VALUES (42)"); err != nil {
		t.Fatalf("INSERT as the provisioned tenant user into its own database: %v", err)
	}
}

// TestProvisionedUserCannotReadOtherTenantDatabase is one of the four
// adversarial probes /docs/phase-4-isolation-design.md's verification
// plan names for Phase 4 task 8 (see api/queryapi/
// tenant_isolation_gap_test.go, which stubs this exact scenario as
// blocked pending tenantprovision existing) -- now that tenantprovision
// exists, this is the first one that can actually run for real.
func TestProvisionedUserCannotReadOtherTenantDatabase(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	ctx := context.Background()

	tenantA := testTenantID()
	tenantB := testTenantID()
	credsA, err := p.ProvisionClickHouse(ctx, tenantA)
	if err != nil {
		t.Fatalf("provisioning tenant A: %v", err)
	}
	if _, err := p.ProvisionClickHouse(ctx, tenantB); err != nil {
		t.Fatalf("provisioning tenant B: %v", err)
	}

	// Seed a row in tenant B's database as admin.
	if err := admin.Exec(ctx, fmt.Sprintf("CREATE TABLE `%s`.secret (id UInt8) ENGINE = Memory", tenantB)); err != nil {
		t.Fatalf("creating table in tenant B's database: %v", err)
	}
	if err := admin.Exec(ctx, fmt.Sprintf("INSERT INTO `%s`.secret VALUES (1)", tenantB)); err != nil {
		t.Fatalf("inserting into tenant B's database: %v", err)
	}

	tenantAConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_ADDR")},
		Auth: clickhouse.Auth{Database: tenantA, Username: credsA.Username, Password: credsA.Password},
	})
	if err != nil {
		t.Fatalf("opening tenant A connection: %v", err)
	}
	defer tenantAConn.Close()

	// The core adversarial probe: tenant A's user attempting to read
	// tenant B's database by fully-qualified name in raw SQL.
	err = tenantAConn.Exec(ctx, fmt.Sprintf("SELECT * FROM `%s`.secret", tenantB))
	if err == nil {
		t.Fatal("tenant A's user was able to read tenant B's database -- isolation is broken")
	}
}

// TestProvisionedUserCannotReadSystemTables is item 2 of
// /docs/phase-4-isolation-design.md's verification plan (see
// api/queryapi/tenant_isolation_gap_test.go for the other three items'
// status) -- task 2's finding was that system.* visibility for a
// non-admin ClickHouse user is version-dependent and must be checked
// live, not assumed from documentation. ProvisionClickHouse never
// explicitly grants system.* access to anything (see its doc comment);
// this test is what actually confirms that omission is sufficient on
// the ClickHouse version this repo pins (docker-compose.yml:
// clickhouse/clickhouse-server:24.8), rather than trusting the omission
// alone.
func TestProvisionedUserCannotReadSystemTables(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	tenantID := testTenantID()
	creds, err := p.ProvisionClickHouse(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ProvisionClickHouse: %v", err)
	}

	tenantConn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{os.Getenv("TENANTPROVISION_TEST_CLICKHOUSE_ADDR")},
		Auth: clickhouse.Auth{Database: tenantID, Username: creds.Username, Password: creds.Password},
	})
	if err != nil {
		t.Fatalf("opening tenant connection: %v", err)
	}
	defer tenantConn.Close()

	// system.query_log/system.tables: expect a hard access-denied error,
	// not a filtered/empty result -- these tables contain other
	// tenants' query text and schema, so "succeeds but happens to
	// return nothing for this user" would still be a version-dependent
	// assumption worth catching, not something this test treats as a pass.
	for _, probe := range []string{
		"SELECT * FROM system.query_log LIMIT 1",
		"SELECT * FROM system.tables LIMIT 1",
	} {
		if err := tenantConn.Exec(context.Background(), probe); err == nil {
			t.Errorf("tenant user was able to run %q -- system.* access was not actually revoked on this ClickHouse version", probe)
		}
	}

	// SHOW DATABASES is checked differently: some ClickHouse versions
	// filter this to only databases the user can see rather than
	// erroring outright, which is an acceptable outcome for this
	// specific statement (unlike query_log/tables above) as long as it
	// doesn't reveal other tenants' database names.
	rows, err := tenantConn.Query(context.Background(), "SHOW DATABASES")
	if err != nil {
		return // erroring outright is also an acceptable outcome here.
	}
	defer rows.Close()
	for rows.Next() {
		var db string
		if err := rows.Scan(&db); err != nil {
			t.Fatalf("scanning SHOW DATABASES row: %v", err)
		}
		if db != tenantID && db != "default" && db != "system" && db != "INFORMATION_SCHEMA" && db != "information_schema" {
			t.Errorf("SHOW DATABASES revealed a database this tenant user shouldn't see: %q", db)
		}
	}
}

func TestProvisionClickHouseRejectsUnsafeTenantID(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	if _, err := p.ProvisionClickHouse(context.Background(), "not safe; DROP TABLE x"); err == nil {
		t.Fatal("expected ProvisionClickHouse to reject an unsafe tenant identifier")
	}
}

func TestProvisionClickHouseSecondCallForSameTenantFails(t *testing.T) {
	admin := testAdminConn(t)
	p := New(admin)
	tenantID := testTenantID()
	ctx := context.Background()

	if _, err := p.ProvisionClickHouse(ctx, tenantID); err != nil {
		t.Fatalf("first ProvisionClickHouse: %v", err)
	}
	if _, err := p.ProvisionClickHouse(ctx, tenantID); err == nil {
		t.Fatal("expected a second ProvisionClickHouse call for the same tenant to fail -- see the function's doc comment on why re-provisioning must not silently succeed")
	}
}
