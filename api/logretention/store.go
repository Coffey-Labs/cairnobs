// Package logretention lets an owner or admin permanently delete log
// records older than a chosen age, scoped to specific hosts -- deleting
// by age alone (with no way to target which agents' logs) turned out
// to be a real footgun for an operator who only wants to clean up one
// noisy host, not everything; storage/README.md has flagged "no
// TTL/retention clause yet" since Phase 0, this is the on-demand,
// operator-triggered, host-scoped half of that gap (not an automatic
// TTL, which is a different, engine-driven design nobody asked for
// here).
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
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Store issues purpose-built, parameterized statements against the
// `logs` table -- deliberately not querylang/executor.ChRunner, whose
// one method (RunSQL) is scoped to arbitrary SELECT statements for the
// query language compiler. This package only ever needs a handful of
// fixed statement shapes (list hosts, count, delete), so keeping them
// separate avoids stretching ChRunner's SELECT-shaped contract to also
// cover a DML mutation.
type Store struct {
	conn driver.Conn
}

func NewStore(conn driver.Conn) *Store {
	return &Store{conn: conn}
}

type HostCount struct {
	Host  string `json:"host"`
	Count uint64 `json:"count"`
}

// HostsOlderThan lists every host with at least one log record older
// than cutoff, along with how many -- backs the host picker a caller
// selects from before previewing/deleting, so the list only ever shows
// hosts that actually have something to act on for the chosen age.
func (s *Store) HostsOlderThan(ctx context.Context, cutoff time.Time) ([]HostCount, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT host, count() AS n FROM logs WHERE timestamp < ? GROUP BY host ORDER BY n DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostCount
	for rows.Next() {
		var hc HostCount
		if err := rows.Scan(&hc.Host, &hc.Count); err != nil {
			return nil, err
		}
		out = append(out, hc)
	}
	return out, rows.Err()
}

// hostPlaceholders builds "?, ?, ..." for n hosts and the matching
// []any argument slice (cutoff first, then each host) -- shared by
// CountOlderThan and DeleteOlderThan since both statements have the
// same "timestamp < ? AND host IN (...)" shape. Callers must never
// pass an empty hosts slice (an empty IN () is invalid SQL, and more
// importantly "no hosts specified" must never silently mean "every
// host" -- see Handler.parseHosts, which rejects that before this is
// ever called).
func hostPlaceholders(cutoff time.Time, hosts []string) (string, []any) {
	placeholders := make([]string, len(hosts))
	args := make([]any, 0, len(hosts)+1)
	args = append(args, cutoff)
	for i, h := range hosts {
		placeholders[i] = "?"
		args = append(args, h)
	}
	return strings.Join(placeholders, ", "), args
}

// CountOlderThan reports how many log records from any of hosts are
// older than cutoff -- backs the "this will delete N records" preview
// a caller shows before asking for confirmation.
func (s *Store) CountOlderThan(ctx context.Context, cutoff time.Time, hosts []string) (uint64, error) {
	ph, args := hostPlaceholders(cutoff, hosts)
	row := s.conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM logs WHERE timestamp < ? AND host IN (%s)", ph), args...)
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOlderThan issues a synchronous ClickHouse mutation
// (SETTINGS mutations_sync = 1) deleting every log record from any of
// hosts older than cutoff. Synchronous rather than fire-and-forget so
// a 200 response means the data is actually gone, not just queued -- an
// owner/admin confirming a permanent delete should be able to trust the
// response. This does block for as long as the mutation takes, which
// could be a while against a very large table; a disclosed tradeoff for
// this deployment's homelab/small-scale target, not a hidden one.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time, hosts []string) error {
	ph, args := hostPlaceholders(cutoff, hosts)
	return s.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE logs DELETE WHERE timestamp < ? AND host IN (%s) SETTINGS mutations_sync = 1", ph), args...)
}
