// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package state

// Workloads from cluster agents (docs/cluster-watcher.md, Storing it): one
// row per node, pod, VM or volume, written by whichever copy took the
// batch. A removed one stays as a tombstone (gone) for a while, so other
// copies learn of it. changed_ms comes from the database's clock, so every
// copy reads "what changed since" on one clock.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const workloadsSchema = `
CREATE TABLE IF NOT EXISTS workloads (
	source TEXT NOT NULL, id TEXT NOT NULL, kind TEXT NOT NULL, record TEXT NOT NULL,
	generation TEXT NOT NULL, gone INTEGER NOT NULL, changed_ms BIGINT NOT NULL,
	PRIMARY KEY (source, id)
);
CREATE INDEX IF NOT EXISTS workloads_changed ON workloads (changed_ms);
CREATE TABLE IF NOT EXISTS workload_sources (
	source TEXT PRIMARY KEY, heard_ms BIGINT NOT NULL, generation TEXT NOT NULL,
	listed_ms BIGINT NOT NULL, agent TEXT NOT NULL, changed_ms BIGINT NOT NULL
);
`

// nowMS is the database's clock in milliseconds, as SQL.
func (k dialect) nowMS() string {
	if k == postgres {
		return `(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT`
	}
	return `CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`
}

// WorkloadRecord is one workload to store: its record as JSON.
type WorkloadRecord struct {
	ID     string
	Kind   string
	Record []byte
}

// WorkloadBatch is what came in one request from a cluster agent.
type WorkloadBatch struct {
	Source     string
	Agent      string // its build version
	Generation string
	Full       string // begin, page, end or ""
	Records    []WorkloadRecord
	Removed    []string
	Heard      time.Time
}

// PutWorkloads stores a batch in one transaction. A record that hasn't
// changed keeps its changed_ms, so a full listing of 10,000 unchanged
// workloads isn't read again by every copy. The end of a full listing
// marks every row of the source that it didn't include as gone.
func (d *DB) PutWorkloads(b WorkloadBatch) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	now := d.kind.nowMS()
	// Keep the last of each id: one statement can't touch a row twice.
	seen := map[string]int{}
	recs := make([]WorkloadRecord, 0, len(b.Records))
	for _, r := range b.Records {
		if i, ok := seen[r.ID]; ok {
			recs[i] = r
			continue
		}
		seen[r.ID] = len(recs)
		recs = append(recs, r)
	}
	for len(recs) > 0 {
		chunk := recs[:min(len(recs), 200)]
		recs = recs[len(chunk):]
		var q strings.Builder
		q.WriteString(`INSERT INTO workloads (source, id, kind, record, generation, gone, changed_ms) VALUES `)
		args := make([]any, 0, len(chunk)*5)
		for i, r := range chunk {
			if i > 0 {
				q.WriteString(", ")
			}
			q.WriteString(`(?, ?, ?, ?, ?, 0, ` + now + `)`)
			args = append(args, b.Source, r.ID, r.Kind, string(r.Record), b.Generation)
		}
		q.WriteString(` ON CONFLICT (source, id) DO UPDATE SET kind = excluded.kind, record = excluded.record,
			generation = excluded.generation, gone = 0,
			changed_ms = CASE WHEN workloads.record = excluded.record AND workloads.gone = 0 THEN workloads.changed_ms ELSE excluded.changed_ms END`)
		if _, err := t.Exec(q.String(), args...); err != nil {
			return err
		}
	}
	for _, id := range b.Removed {
		if _, err := t.Exec(`UPDATE workloads SET gone = 1, changed_ms = `+now+` WHERE source = ? AND id = ? AND gone = 0`, b.Source, id); err != nil {
			return err
		}
	}
	listed, listedMS := "workload_sources.listed_ms", int64(0)
	if b.Full == "end" {
		if _, err := t.Exec(`UPDATE workloads SET gone = 1, changed_ms = `+now+` WHERE source = ? AND generation <> ? AND gone = 0`, b.Source, b.Generation); err != nil {
			return err
		}
		listed, listedMS = "excluded.listed_ms", b.Heard.UnixMilli()
	}
	if _, err := t.Exec(`INSERT INTO workload_sources (source, heard_ms, generation, listed_ms, agent, changed_ms) VALUES (?, ?, ?, ?, ?, `+now+`)
		ON CONFLICT (source) DO UPDATE SET heard_ms = excluded.heard_ms, generation = excluded.generation,
		listed_ms = `+listed+`, agent = excluded.agent, changed_ms = excluded.changed_ms`,
		b.Source, b.Heard.UnixMilli(), b.Generation, listedMS, b.Agent); err != nil {
		return err
	}
	return t.Commit()
}

// WorkloadRow is a stored workload. Changed is on the database's clock.
type WorkloadRow struct {
	Source  string
	ID      string
	Kind    string
	Record  []byte
	Gone    bool
	Changed int64 // ms
}

// WorkloadsSince reads every row changed after ms (on the database's
// clock), tombstones included. All of them, with ms < 0, leaves the
// tombstones out.
func (d *DB) WorkloadsSince(ms int64) ([]WorkloadRow, error) {
	q := `SELECT source, id, kind, record, gone, changed_ms FROM workloads WHERE changed_ms > ?`
	if ms < 0 {
		q += ` AND gone = 0`
	}
	rows, err := d.query(q, ms)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkloadRow
	for rows.Next() {
		var w WorkloadRow
		var rec string
		var gone int
		if err := rows.Scan(&w.Source, &w.ID, &w.Kind, &rec, &gone, &w.Changed); err != nil {
			return nil, err
		}
		w.Record, w.Gone = []byte(rec), gone != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkloadSource is what's known of one cluster agent.
type WorkloadSource struct {
	Source     string
	Heard      time.Time // its last batch
	Generation string
	Listed     time.Time // the end of its last full listing; zero before the first
	Agent      string
}

// WorkloadSources reads every cluster agent's state.
func (d *DB) WorkloadSources() ([]WorkloadSource, error) {
	rows, err := d.query(`SELECT source, heard_ms, generation, listed_ms, agent FROM workload_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkloadSource
	for rows.Next() {
		var s WorkloadSource
		var heard, listed int64
		if err := rows.Scan(&s.Source, &heard, &s.Generation, &listed, &s.Agent); err != nil {
			return nil, err
		}
		s.Heard = time.UnixMilli(heard)
		if listed > 0 {
			s.Listed = time.UnixMilli(listed)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DBNowMS is the database's clock, in ms.
func (d *DB) DBNowMS() (int64, error) {
	var ms int64
	err := d.queryRow(`SELECT ` + d.kind.nowMS()).Scan(&ms)
	return ms, err
}

// PurgeWorkloads forgets tombstones older than before (on the database's
// clock). A copy that was away longer reads everything again.
func (d *DB) PurgeWorkloads(before int64) (int64, error) {
	res, err := d.exec(`DELETE FROM workloads WHERE gone = 1 AND changed_ms < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteWorkloadSource forgets a cluster agent and everything it sent but
// its history.
func (d *DB) DeleteWorkloadSource(source string) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.Exec(`DELETE FROM workloads WHERE source = ?`, source); err != nil {
		return err
	}
	if _, err := t.Exec(`DELETE FROM workload_sources WHERE source = ?`, source); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Its latest usage goes too; its hours stay until they age out.
	if _, err := t.Exec(`DELETE FROM workload_usage WHERE source = ?`, source); err != nil {
		return err
	}
	return t.Commit()
}
