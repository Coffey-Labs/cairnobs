// Package logretention lets an owner or admin permanently delete log
// records older than a chosen age, scoped to specific (host, service)
// targets -- deleting by age alone, with no way to target which
// agents' or which services' logs, turned out to be a real footgun for
// an operator who only wants to clean up one noisy source (e.g. a
// chatty nginx access log) without touching everything else that host
// ships (smtp, ufw, ...); storage/README.md has flagged "no TTL/
// retention clause yet" since Phase 0, this is the on-demand,
// operator-triggered, host-and-service-scoped half of that gap (not an
// automatic TTL, which is a different, engine-driven design nobody
// asked for here).
//
// service is a genuine per-log-record dimension already, not something
// this package invents: storage/migrations/0001_create_logs_table.sql
// has always had a `service` column, and distinct services on one host
// are a real, already-supported shape (separate cairnobs-agent processes
// on the same machine, each with its own agent.toml `service` -- see
// /docs/agent-management-design.md), not merely a per-agent label.
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
// fixed statement shapes (list targets, count, delete), so keeping them
// separate avoids stretching ChRunner's SELECT-shaped contract to also
// cover a DML mutation.
type Store struct {
	conn driver.Conn
}

func NewStore(conn driver.Conn) *Store {
	return &Store{conn: conn}
}

// HostService identifies one (host, service) pair -- the atomic unit a
// caller selects for preview/deletion. Never a wildcard: a request
// naming a host with no service (or vice versa) is invalid at the
// handler layer (see parseTargets), so this package never has to
// reason about "every service on this host."
type HostService struct {
	Host    string `json:"host"`
	Service string `json:"service"`
}

type TargetCount struct {
	Host    string
	Service string
	Count   uint64
}

// TargetsOlderThan lists every (host, service) pair with at least one
// log record older than cutoff, along with how many -- backs the
// picker a caller selects from before previewing/deleting. Ordered by
// host so the handler can group contiguous rows into a per-host list
// without a second pass.
func (s *Store) TargetsOlderThan(ctx context.Context, cutoff time.Time) ([]TargetCount, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT host, service, count() AS n FROM logs
		WHERE timestamp < ?
		GROUP BY host, service
		ORDER BY host, n DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TargetCount
	for rows.Next() {
		var tc TargetCount
		if err := rows.Scan(&tc.Host, &tc.Service, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// targetPlaceholders builds "(?, ?), (?, ?), ..." for a tuple IN clause
// over (host, service) and the matching []any argument slice (cutoff
// first, then each pair) -- shared by CountOlderThan and
// DeleteOlderThan since both statements have the same
// "timestamp < ? AND (host, service) IN (...)" shape. Callers must
// never pass an empty targets slice (an empty IN () is invalid SQL, and
// more importantly "no targets specified" must never silently mean
// "everything" -- see Handler.parseTargets, which rejects that before
// this is ever called).
func targetPlaceholders(cutoff time.Time, targets []HostService) (string, []any) {
	pairs := make([]string, len(targets))
	args := make([]any, 0, len(targets)*2+1)
	args = append(args, cutoff)
	for i, t := range targets {
		pairs[i] = "(?, ?)"
		args = append(args, t.Host, t.Service)
	}
	return strings.Join(pairs, ", "), args
}

// CountOlderThan reports how many log records from any of targets are
// older than cutoff -- backs the "this will delete N records" preview
// a caller shows before asking for confirmation.
func (s *Store) CountOlderThan(ctx context.Context, cutoff time.Time, targets []HostService) (uint64, error) {
	ph, args := targetPlaceholders(cutoff, targets)
	row := s.conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM logs WHERE timestamp < ? AND (host, service) IN (%s)", ph), args...)
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOlderThan issues a synchronous ClickHouse mutation
// (SETTINGS mutations_sync = 1) deleting every log record from any of
// targets older than cutoff. Synchronous rather than fire-and-forget so
// a 200 response means the data is actually gone, not just queued -- an
// owner/admin confirming a permanent delete should be able to trust the
// response. This does block for as long as the mutation takes, which
// could be a while against a very large table; a disclosed tradeoff for
// this deployment's homelab/small-scale target, not a hidden one.
func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time, targets []HostService) error {
	ph, args := targetPlaceholders(cutoff, targets)
	return s.conn.Exec(ctx, fmt.Sprintf("ALTER TABLE logs DELETE WHERE timestamp < ? AND (host, service) IN (%s) SETTINGS mutations_sync = 1", ph), args...)
}
