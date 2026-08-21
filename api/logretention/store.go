// Package logretention lets an owner or admin permanently delete log
// records older than a chosen age -- storage/README.md has flagged "no
// TTL/retention clause yet" since Phase 0; this is the on-demand,
// operator-triggered half of that gap (not an automatic TTL, which is
// a different, engine-driven design nobody asked for here).
//
// Deliberately scoped to core's single ClickHouse `logs` table, not
// enterprise/'s per-tenant ClickHouse routing
// (enterprise/internal/chrunner.Registry) -- core has no tenant_id
// column on `logs` at all (tenant isolation there lives at the
// connection layer per /docs/phase-4-isolation-design.md), so there is
// nothing to scope a single-tenant deletion by. A tenant-aware
// equivalent for enterprise/ is real, disclosed future work, not
// silently assumed to already work there.
//
// Also disclosed, not silently ignored: deleting from ClickHouse does
// not prune the Tantivy full-text index (search/) -- that index has no
// timestamp field and no bulk/range-delete primitive today (only a
// per-record upsert), so a deleted record's record_id can keep
// resolving to nothing via free-text search until search/ grows a real
// deletion path. Closing that gap is a separate, larger piece of work
// spanning proto/search.proto, search/src/grpc.rs, and
// api/searchclient -- out of scope for this feature.
package logretention

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Store issues purpose-built, parameterized statements against the
// `logs` table -- deliberately not querylang/executor.ChRunner, whose
// one method (RunSQL) is scoped to arbitrary SELECT statements for the
// query language compiler. This package only ever needs two fixed
// statements (a count and a delete), so keeping them separate avoids
// stretching ChRunner's SELECT-shaped contract to also cover a DML
// mutation.
type Store struct {
	conn driver.Conn
}

func NewStore(conn driver.Conn) *Store {
	return &Store{conn: conn}
}

// CountOlderThan reports how many log records are older than cutoff --
// backs the "this will delete N records" preview a caller shows before
// asking for confirmation.
func (s *Store) CountOlderThan(ctx context.Context, cutoff time.Time) (uint64, error) {
	row := s.conn.QueryRow(ctx, "SELECT count() FROM logs WHERE timestamp < ?", cutoff)
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOlderThan issues a synchronous ClickHouse mutation
// (SETTINGS mutations_sync = 1) deleting every log record older than
// cutoff. Synchronous rather than fire-and-forget so a 200 response
// means the data is actually gone, not just queued -- an owner/admin
// confirming a permanent delete should be able to trust the response.
// This does block for as long as the mutation takes, which could be a
// while against a very large table; a disclosed tradeoff for this
// deployment's homelab/small-scale target, not a hidden one.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) error {
	return s.conn.Exec(ctx, "ALTER TABLE logs DELETE WHERE timestamp < ? SETTINGS mutations_sync = 1", cutoff)
}
