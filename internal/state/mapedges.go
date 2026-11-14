package state

// Map edges: the dependencies learned from connections (docs/maps.md,
// Learned edges). One row per edge between two services on a port, with
// the hours it was seen in, so the maps can tell an edge seen once from a
// steady one. The evaluator writes, at most once an hour per edge; every
// copy reads.

import (
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

const mapEdgesSchema = `
CREATE TABLE IF NOT EXISTS map_edges (
	src TEXT NOT NULL, dst TEXT NOT NULL, port INTEGER NOT NULL,
	first_ms BIGINT NOT NULL, last_ms BIGINT NOT NULL,
	hours TEXT NOT NULL, steady_ms BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (src, dst, port)
);
`

// EdgeKey names a learned edge.
type EdgeKey struct {
	Src, Dst string
	Port     int
}

// EdgeRow is a learned edge as stored.
type EdgeRow struct {
	EdgeKey
	First, Last time.Time
	Hours       []int64   // the most recent distinct hours it was seen in (Unix hours), oldest first
	Steady      time.Time // when it became steady; zero: not yet
}

// Edge hours: keep this many, and call an edge steady once it's been seen
// in SteadyHours separate hours within SteadyWithin.
const (
	keepHours    = 8
	SteadyHours  = 3
	SteadyWithin = 7 * 24 * time.Hour
)

// steadyIn says whether hours (sorted) hold SteadyHours within SteadyWithin.
func steadyIn(hours []int64) bool {
	span := int64(SteadyWithin / time.Hour)
	for i := 0; i+SteadyHours-1 < len(hours); i++ {
		if hours[i+SteadyHours-1]-hours[i] < span {
			return true
		}
	}
	return false
}

// Seen notes that the edge was seen at the given time: its last sighting,
// its hours, and whether that makes it steady. A sighting older than the
// last changes nothing.
func (r *EdgeRow) Seen(at time.Time) {
	if at.Before(r.Last) {
		return
	}
	if r.First.IsZero() {
		r.First = at
	}
	r.Last = at
	hour := at.Unix() / 3600
	if !slices.Contains(r.Hours, hour) {
		r.Hours = append(r.Hours, hour)
		slices.Sort(r.Hours)
		if len(r.Hours) > keepHours {
			r.Hours = r.Hours[len(r.Hours)-keepHours:]
		}
	}
	if r.Steady.IsZero() && steadyIn(r.Hours) {
		r.Steady = at
	}
}

// SeenEdges records that each edge was seen at the given time.
func (d *DB) SeenEdges(keys []EdgeKey, at time.Time) error {
	if len(keys) == 0 {
		return nil
	}
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	for _, k := range keys {
		r := EdgeRow{EdgeKey: k}
		var first, last, steady int64
		var hs string
		err := t.QueryRow(`SELECT first_ms, last_ms, hours, steady_ms FROM map_edges WHERE src = ? AND dst = ? AND port = ?`,
			k.Src, k.Dst, k.Port).Scan(&first, &last, &hs, &steady)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			r.First, r.Last, r.Hours = time.UnixMilli(first), time.UnixMilli(last), parseHours(hs)
			if steady != 0 {
				r.Steady = time.UnixMilli(steady)
			}
		}
		if at.Before(r.Last) {
			continue // late, from a copy that lost the lease
		}
		r.Seen(at)
		steady = 0
		if !r.Steady.IsZero() {
			steady = r.Steady.UnixMilli()
		}
		if _, err := t.Exec(`INSERT INTO map_edges (src, dst, port, first_ms, last_ms, hours, steady_ms) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (src, dst, port) DO UPDATE SET last_ms = excluded.last_ms, hours = excluded.hours, steady_ms = excluded.steady_ms
			WHERE map_edges.last_ms <= excluded.last_ms`,
			k.Src, k.Dst, k.Port, r.First.UnixMilli(), r.Last.UnixMilli(), formatHours(r.Hours), steady); err != nil {
			return err
		}
	}
	return t.Commit()
}

// Edges reads every learned edge.
func (d *DB) Edges() ([]EdgeRow, error) {
	rows, err := d.query(`SELECT src, dst, port, first_ms, last_ms, hours, steady_ms FROM map_edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EdgeRow
	for rows.Next() {
		var r EdgeRow
		var first, last, steady int64
		var hs string
		if err := rows.Scan(&r.Src, &r.Dst, &r.Port, &first, &last, &hs, &steady); err != nil {
			return nil, err
		}
		r.First, r.Last, r.Hours = time.UnixMilli(first), time.UnixMilli(last), parseHours(hs)
		if steady != 0 {
			r.Steady = time.UnixMilli(steady)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeEdges drops edges last seen before the given time.
func (d *DB) PurgeEdges(before time.Time) (int64, error) {
	res, err := d.exec(`DELETE FROM map_edges WHERE last_ms < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func parseHours(s string) []int64 {
	var out []int64
	for f := range strings.SplitSeq(s, ",") {
		if n, err := strconv.ParseInt(f, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func formatHours(h []int64) string {
	parts := make([]string, len(h))
	for i, n := range h {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(parts, ",")
}

// Map services: when each discovered service was first and last seen, so
// the service map can list what's new ("Found since you last looked").
// Written by the evaluator at most once an hour per service, like edges.
const mapServicesSchema = `
CREATE TABLE IF NOT EXISTS map_services (
	id TEXT PRIMARY KEY, first_ms BIGINT NOT NULL, last_ms BIGINT NOT NULL
);
`

// ServiceRow is a discovered service's sightings.
type ServiceRow struct {
	ID          string
	First, Last time.Time
}

// SeenServices records that each service was seen at the given time.
func (d *DB) SeenServices(ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	ms := at.UnixMilli()
	for _, id := range ids {
		if _, err := t.Exec(`INSERT INTO map_services (id, first_ms, last_ms) VALUES (?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET last_ms = excluded.last_ms WHERE map_services.last_ms <= excluded.last_ms`,
			id, ms, ms); err != nil {
			return err
		}
	}
	return t.Commit()
}

// MapServices reads every service's sightings.
func (d *DB) MapServices() ([]ServiceRow, error) {
	rows, err := d.query(`SELECT id, first_ms, last_ms FROM map_services`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRow
	for rows.Next() {
		var r ServiceRow
		var first, last int64
		if err := rows.Scan(&r.ID, &first, &last); err != nil {
			return nil, err
		}
		r.First, r.Last = time.UnixMilli(first), time.UnixMilli(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeServices drops services last seen before the given time.
func (d *DB) PurgeServices(before time.Time) (int64, error) {
	res, err := d.exec(`DELETE FROM map_services WHERE last_ms < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
