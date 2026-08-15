// Fail-closed behavior (empty/unknown tenant_id) needs no live
// ClickHouse at all -- Registry.WriteBatch returns before ever touching
// a connection for those cases, so those tests run unconditionally.
// Everything that actually writes data is a real integration test
// against a live ClickHouse (same CHWRITER_TEST_CLICKHOUSE_ADDR
// convention as enterprise/internal/chrunner's own tests), skipped
// unless that's set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e CHWRITER_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
//	  -e CHWRITER_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
//	  golang:1.25-alpine go test ./internal/chwriter/... -v
package chwriter

import (
	"context"
	"fmt"
	"os"
	"testing"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/sentry/sentry/enterprise/internal/tenantprovision"
	"github.com/sentry/sentry/ingest/clickhousewriter"
	"github.com/sentry/sentry/ingest/consumer"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// TestWriteBatchRefusesEmptyTenantID and
// TestWriteBatchRefusesUnknownTenantWithEmptyRegistry construct a
// Registry directly (bypassing New, which would dial ClickHouse) so
// they genuinely run without Docker: WriteBatch's fail-closed checks
// happen before ever touching a real connection, purely a map lookup.
func TestWriteBatchRefusesEmptyTenantID(t *testing.T) {
	reg := &Registry{writers: map[string]*clickhousewriter.Writer{}}
	err := reg.WriteBatch(context.Background(), []consumer.Record{
		{TenantID: "", Record: &logsv1.LogRecord{Message: "untagged"}},
	})
	if err == nil {
		t.Fatal("expected WriteBatch to refuse a record with no tenant_id, not silently drop the tag")
	}
}

func TestWriteBatchRefusesUnknownTenantWithEmptyRegistry(t *testing.T) {
	reg := &Registry{writers: map[string]*clickhousewriter.Writer{}}
	err := reg.WriteBatch(context.Background(), []consumer.Record{
		{TenantID: "acme", Record: &logsv1.LogRecord{Message: "m"}},
	})
	if err == nil {
		t.Fatal("expected WriteBatch to refuse a tenant with no entry in the registry")
	}
}

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("CHWRITER_TEST_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Skip("CHWRITER_TEST_CLICKHOUSE_ADDR not set -- skipping live-ClickHouse integration test")
	}
	return addr
}

func provisionTestTenant(t *testing.T, addr string) (tenantID string, creds tenantprovision.Credentials) {
	t.Helper()
	admin, err := chdriver.Open(&chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{Database: "default", Username: "default", Password: os.Getenv("CHWRITER_TEST_CLICKHOUSE_PASSWORD")},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	tenantID = "cw" + uuid.NewString()[:8]
	creds, err = tenantprovision.New(admin).ProvisionClickHouse(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("provisioning tenant %s: %v", tenantID, err)
	}
	if err := admin.Exec(context.Background(), fmt.Sprintf(
		"CREATE TABLE `%s`.logs (timestamp DateTime64(9), host String, service String, severity String, message String, attributes Map(String, String), record_id UUID) ENGINE = MergeTree ORDER BY timestamp",
		tenantID)); err != nil {
		t.Fatalf("creating logs table for tenant %s: %v", tenantID, err)
	}
	return tenantID, creds
}

// TestRegistryWritesEachTenantToItsOwnDatabase is the core adversarial
// probe for the write side, complementing chrunner's own read-side
// version: two tenants, two connections inside one Registry, one
// WriteBatch call mixing records from both, and a direct check (via an
// admin connection, not through Registry) that each tenant's row landed
// only in its own database.
func TestRegistryWritesEachTenantToItsOwnDatabase(t *testing.T) {
	addr := testAddr(t)
	ctx := context.Background()
	tenantA, credsA := provisionTestTenant(t, addr)
	tenantB, credsB := provisionTestTenant(t, addr)

	reg, err := New(ctx, addr, []DataSource{
		{TenantID: tenantA, Database: tenantA, Username: credsA.Username, Password: credsA.Password},
		{TenantID: tenantB, Database: tenantB, Username: credsB.Username, Password: credsB.Password},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	err = reg.WriteBatch(ctx, []consumer.Record{
		{TenantID: tenantA, Record: &logsv1.LogRecord{Host: "h1", Message: "for-a", RecordId: uuid.NewString()}},
		{TenantID: tenantB, Record: &logsv1.LogRecord{Host: "h1", Message: "for-b", RecordId: uuid.NewString()}},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	admin, err := chdriver.Open(&chdriver.Options{
		Addr: []string{addr},
		Auth: chdriver.Auth{Database: "default", Username: "default", Password: os.Getenv("CHWRITER_TEST_CLICKHOUSE_PASSWORD")},
	})
	if err != nil {
		t.Fatalf("opening admin connection: %v", err)
	}
	defer admin.Close()

	for tenantID, wantMessage := range map[string]string{tenantA: "for-a", tenantB: "for-b"} {
		row := admin.QueryRow(ctx, fmt.Sprintf("SELECT message FROM `%s`.logs", tenantID))
		var got string
		if err := row.Scan(&got); err != nil {
			t.Fatalf("querying %s's logs: %v", tenantID, err)
		}
		if got != wantMessage {
			t.Fatalf("tenant %s's logs.message = %q, want %q", tenantID, got, wantMessage)
		}
	}
}

func TestRegistryRefusesUnprovisionedTenant(t *testing.T) {
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

	err = reg.WriteBatch(ctx, []consumer.Record{
		{TenantID: "some-other-tenant-never-provisioned", Record: &logsv1.LogRecord{Host: "h1", Message: "m", RecordId: uuid.NewString()}},
	})
	if err == nil {
		t.Fatal("expected WriteBatch to refuse a tenant with no provisioned connection, not silently drop or misroute it")
	}
}
