// Integration tests against a real Postgres, authenticated as the real
// audit_writer role -- this package's whole point is a set of guarantees
// (grants, the trigger, hash-chain correctness under concurrency) that a
// mocked pgxpool can't actually exercise. Skipped unless
// AUDIT_TEST_POSTGRES_ADDR is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/enterprise \
//	  -e AUDIT_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e AUDIT_TEST_POSTGRES_PASSWORD=audit-writer-dev-only \
//	  -e AUDIT_TEST_ADMIN_PASSWORD=sentry-dev-only \
//	  golang:1.25-alpine go test ./internal/audit/... -v
package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T, user, password string) *pgxpool.Pool {
	t.Helper()
	addr := os.Getenv("AUDIT_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("AUDIT_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s/sentry_metadata", user, password, addr)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func cleanupAuditLog(t *testing.T, adminPool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Errors here were previously swallowed (_, _ =) -- that hid the
	// real cause of a test failure (rows accumulating across test runs)
	// behind what looked like a row-count/ID-assumption bug instead.
	// Surface them.
	if _, err := adminPool.Exec(ctx, "ALTER TABLE audit_log DISABLE TRIGGER audit_log_immutable"); err != nil {
		t.Fatalf("cleanup: disabling trigger: %v", err)
	}
	tag, err := adminPool.Exec(ctx, "DELETE FROM audit_log")
	if err != nil {
		t.Fatalf("cleanup: deleting rows: %v", err)
	}
	t.Logf("cleanup: deleted %d pre-existing rows", tag.RowsAffected())
	if _, err := adminPool.Exec(ctx, "ALTER TABLE audit_log ENABLE TRIGGER audit_log_immutable"); err != nil {
		t.Fatalf("cleanup: re-enabling trigger: %v", err)
	}
}

func TestAppendAndVerifyChainRealPostgres(t *testing.T) {
	writerPool := testPool(t, "audit_writer", os.Getenv("AUDIT_TEST_POSTGRES_PASSWORD"))
	adminPool := testPool(t, "sentry", os.Getenv("AUDIT_TEST_ADMIN_PASSWORD"))
	cleanupAuditLog(t, adminPool)
	defer cleanupAuditLog(t, adminPool)

	store := NewStore(writerPool)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		q := fmt.Sprintf("service=api | stats count %d", i)
		rec, err := store.Append(ctx, Entry{
			TenantID: "default", Source: SourceAPI, EventType: EventQuery,
			QueryText: &q, Status: StatusSuccess,
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if rec.RowHash == "" {
			t.Fatalf("expected a non-empty row hash")
		}
	}

	result, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !result.OK {
		t.Fatalf("expected an intact chain, got broken at id=%d after %d rows checked", result.FirstBadID, result.RowsChecked)
	}
	if result.RowsChecked != 5 {
		t.Fatalf("RowsChecked = %d, want 5", result.RowsChecked)
	}
}

// TestVerifyChainDetectsTampering proves the chain actually catches an
// in-place row modification -- not just that VerifyChain runs without
// erroring on untampered data, which a bug returning OK unconditionally
// would also pass.
func TestVerifyChainDetectsTampering(t *testing.T) {
	writerPool := testPool(t, "audit_writer", os.Getenv("AUDIT_TEST_POSTGRES_PASSWORD"))
	adminPool := testPool(t, "sentry", os.Getenv("AUDIT_TEST_ADMIN_PASSWORD"))
	cleanupAuditLog(t, adminPool)
	defer cleanupAuditLog(t, adminPool)

	store := NewStore(writerPool)
	ctx := context.Background()

	var lastID int64
	for i := 0; i < 3; i++ {
		q := "service=api"
		rec, err := store.Append(ctx, Entry{TenantID: "default", Source: SourceAPI, EventType: EventQuery, QueryText: &q, Status: StatusSuccess})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		lastID = rec.ID
	}

	before, err := store.VerifyChain(ctx)
	if err != nil || !before.OK {
		t.Fatalf("expected chain to verify before tampering: ok=%v err=%v", before.OK, err)
	}

	// Simulate tampering: a privileged actor disables the trigger (the
	// same escape hatch confirmed live in the design doc's verification
	// -- this is the "even the trigger doesn't stop a superuser" case)
	// and rewrites a row's status without recomputing the hash chain.
	if _, err := adminPool.Exec(ctx, "ALTER TABLE audit_log DISABLE TRIGGER audit_log_immutable"); err != nil {
		t.Fatalf("disabling trigger for the tamper simulation: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "UPDATE audit_log SET status = 'error' WHERE id = $1", lastID); err != nil {
		t.Fatalf("simulated tamper UPDATE: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "ALTER TABLE audit_log ENABLE TRIGGER audit_log_immutable"); err != nil {
		t.Fatalf("re-enabling trigger: %v", err)
	}

	after, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain after tampering: %v", err)
	}
	if after.OK {
		t.Fatalf("expected VerifyChain to detect the tampered row, got OK")
	}
	if after.FirstBadID != lastID {
		t.Fatalf("FirstBadID = %d, want %d", after.FirstBadID, lastID)
	}
}

// TestAppendConcurrentWritesProduceAValidChain exercises the advisory
// lock: without it, concurrent Append calls could read the same
// prev_hash and fork the chain. Real concurrency, real Postgres, not a
// unit test of the Go code alone.
func TestAppendConcurrentWritesProduceAValidChain(t *testing.T) {
	writerPool := testPool(t, "audit_writer", os.Getenv("AUDIT_TEST_POSTGRES_PASSWORD"))
	adminPool := testPool(t, "sentry", os.Getenv("AUDIT_TEST_ADMIN_PASSWORD"))
	cleanupAuditLog(t, adminPool)
	defer cleanupAuditLog(t, adminPool)

	store := NewStore(writerPool)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf("query-%d", i)
			_, err := store.Append(ctx, Entry{TenantID: "default", Source: SourceAPI, EventType: EventQuery, QueryText: &q, Status: StatusSuccess})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Append failed: %v", err)
		}
	}

	result, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !result.OK {
		t.Fatalf("expected an intact chain after %d concurrent appends, got broken at id=%d", n, result.FirstBadID)
	}
	if result.RowsChecked != n {
		t.Fatalf("RowsChecked = %d, want %d", result.RowsChecked, n)
	}
}

// TestCheckpointerRun ties Store + FileSink together against real
// audit_log data: writes some rows, checkpoints, writes more, checkpoints
// again, and confirms the second checkpoint picks up exactly where the
// first left off (FromID = previous ToID + 1) with a hash chained off
// the previous checkpoint's hash.
func TestCheckpointerRun(t *testing.T) {
	writerPool := testPool(t, "audit_writer", os.Getenv("AUDIT_TEST_POSTGRES_PASSWORD"))
	adminPool := testPool(t, "sentry", os.Getenv("AUDIT_TEST_ADMIN_PASSWORD"))
	cleanupAuditLog(t, adminPool)
	defer cleanupAuditLog(t, adminPool)

	store := NewStore(writerPool)
	sink := NewFileSink(filepath.Join(t.TempDir(), "checkpoints.jsonl"))
	checkpointer := NewCheckpointer(store, sink)
	ctx := context.Background()

	// DELETE doesn't reset the BIGSERIAL sequence, so IDs are not
	// guaranteed to start at 1 -- but Checkpoint.FromID is a *cursor
	// position* (1, or the previous checkpoint's ToID+1), not "the
	// lowest row ID that happens to still exist." In real usage audit_log
	// never has gaps (append-only, protected by the immutability
	// trigger), so those always coincide; here, this test's own
	// destructive cleanupAuditLog between test functions creates a gap
	// (rows from earlier tests were deleted, advancing the sequence)
	// that real usage never produces -- so FromID is asserted against
	// the cursor's own logic (1, since no prior checkpoint exists for
	// this fresh FileSink), and ToID against the actual last ID Append
	// returned.
	var firstBatchLastID int64
	for i := 0; i < 3; i++ {
		q := "first batch"
		rec, err := store.Append(ctx, Entry{TenantID: "default", Source: SourceAPI, EventType: EventQuery, QueryText: &q, Status: StatusSuccess})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		firstBatchLastID = rec.ID
	}

	cp1, err := checkpointer.Run(ctx)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if cp1 == nil {
		t.Fatalf("expected a checkpoint after 3 rows, got nil")
	}
	if cp1.FromID != 1 || cp1.ToID != firstBatchLastID {
		t.Fatalf("cp1 = %+v, want FromID=1 ToID=%d", cp1, firstBatchLastID)
	}

	// Nothing new since the last checkpoint -- Run should be a no-op.
	noop, err := checkpointer.Run(ctx)
	if err != nil {
		t.Fatalf("no-op Run: %v", err)
	}
	if noop != nil {
		t.Fatalf("expected nil (nothing new to checkpoint), got %+v", noop)
	}

	var secondBatchLastID int64
	for i := 0; i < 2; i++ {
		q := "second batch"
		rec, err := store.Append(ctx, Entry{TenantID: "default", Source: SourceAPI, EventType: EventQuery, QueryText: &q, Status: StatusSuccess})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		secondBatchLastID = rec.ID
	}

	cp2, err := checkpointer.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if cp2 == nil {
		t.Fatalf("expected a second checkpoint, got nil")
	}
	if cp2.FromID != cp1.ToID+1 || cp2.ToID != secondBatchLastID {
		t.Fatalf("cp2 = %+v, want FromID=%d ToID=%d", cp2, cp1.ToID+1, secondBatchLastID)
	}
	if cp2.PrevCheckpointHash != cp1.Hash {
		t.Fatalf("cp2.PrevCheckpointHash = %q, want %q (chained to cp1)", cp2.PrevCheckpointHash, cp1.Hash)
	}
}
