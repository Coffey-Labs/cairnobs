// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package state

// The outbox: notices waiting to be sent, one row per notice and channel.
// The evaluator writes a notice here in the same transaction as the
// incident it's about, and sends from here, deleting each once sent. If
// it dies in between, the copy that takes over sends what's left. A
// notice can then go twice (sent, but its copy died before deleting it),
// never not at all (docs/scale-and-availability.md, One evaluator at a
// time).

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

const outboxSchema = `
CREATE TABLE IF NOT EXISTS outbox (
	id TEXT PRIMARY KEY, made BIGINT NOT NULL, channel TEXT NOT NULL, notice TEXT NOT NULL,
	attempts INTEGER NOT NULL, due BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS outbox_due ON outbox (due);
`

// OutboxItem is one notice for one channel. Notice is the notice as JSON.
type OutboxItem struct {
	ID       string
	Made     time.Time
	Channel  string
	Notice   string
	Attempts int
	Due      time.Time
}

func newOutboxID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func putOutbox(t txn, items []OutboxItem) error {
	for _, it := range items {
		if it.ID == "" {
			it.ID = newOutboxID()
		}
		if it.Due.IsZero() {
			it.Due = it.Made
		}
		if _, err := t.Exec(`INSERT INTO outbox (id, made, channel, notice, attempts, due) VALUES (?, ?, ?, ?, ?, ?)`,
			it.ID, it.Made.UnixMilli(), it.Channel, it.Notice, it.Attempts, it.Due.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// QueueNotices puts notices in the outbox on their own (acknowledgements,
// which change no incident here): ErrFenced, and nothing queued, once f's
// lease has passed to someone else. A nil fence writes unconditionally.
func (d *DB) QueueNotices(items []OutboxItem, f *Fence) error {
	if len(items) == 0 {
		return nil
	}
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if f != nil {
		var held bool
		if err := t.QueryRow(`SELECT `+fenced, f.args()...).Scan(&held); err != nil {
			return err
		}
		if !held {
			return ErrFenced
		}
	}
	if err := putOutbox(t, items); err != nil {
		return err
	}
	return t.Commit()
}

// DueNotices lists notices whose time has come, oldest first.
func (d *DB) DueNotices(now time.Time, limit int) ([]OutboxItem, error) {
	rows, err := d.query(`SELECT id, made, channel, notice, attempts, due FROM outbox WHERE due <= ? ORDER BY made, id LIMIT ?`,
		now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var made, due int64
		if err := rows.Scan(&it.ID, &made, &it.Channel, &it.Notice, &it.Attempts, &due); err != nil {
			return nil, err
		}
		it.Made, it.Due = time.UnixMilli(made), time.UnixMilli(due)
		out = append(out, it)
	}
	return out, rows.Err()
}

// NoticeDone takes a notice out of the outbox: sent, or given up on.
func (d *DB) NoticeDone(id string) error {
	_, err := d.exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// NoticeLater counts a failed attempt and sets when to try again.
func (d *DB) NoticeLater(id string, attempts int, due time.Time) error {
	_, err := d.exec(`UPDATE outbox SET attempts = ?, due = ? WHERE id = ?`, attempts, due.UnixMilli(), id)
	return err
}
