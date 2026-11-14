// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package state

// Readings: what obs-beacond polls for itself (Cloudflare, Patroni, the
// mail round trip), one row each. The evaluator polls and writes here;
// every other copy reads the row back, so each outside API is asked once
// however many copies run, and every copy shows the same thing
// (docs/scale-and-availability.md, Sharing live state). A copy that takes
// the evaluator's place starts from the last row, so what a poller
// remembers between rounds (how long Cloudflare has been failing, the
// mail round trip's day of history) carries over.

import (
	"database/sql"
	"errors"
	"time"
)

const readingsSchema = `
CREATE TABLE IF NOT EXISTS readings (
	name TEXT PRIMARY KEY, body TEXT NOT NULL, at_ms BIGINT NOT NULL
);
`

// PutReading stores a poller's latest reading, unless a newer one is there
// already (a copy that just lost the lease may still finish a round).
func (d *DB) PutReading(name string, body []byte, at time.Time) error {
	_, err := d.exec(`INSERT INTO readings (name, body, at_ms) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET body = excluded.body, at_ms = excluded.at_ms
		WHERE readings.at_ms <= excluded.at_ms`, name, string(body), at.UnixMilli())
	return err
}

// Reading reads a poller's latest reading and when it was taken.
func (d *DB) Reading(name string) ([]byte, time.Time, error) {
	var body string
	var ms int64
	err := d.queryRow(`SELECT body, at_ms FROM readings WHERE name = ?`, name).Scan(&body, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	return []byte(body), time.UnixMilli(ms), err
}

// ReadingTimes says when each reading was taken, to find the ones a copy
// has missed.
func (d *DB) ReadingTimes() (map[string]time.Time, error) {
	rows, err := d.query(`SELECT name, at_ms FROM readings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var n string
		var ms int64
		if err := rows.Scan(&n, &ms); err != nil {
			return nil, err
		}
		out[n] = time.UnixMilli(ms)
	}
	return out, rows.Err()
}
