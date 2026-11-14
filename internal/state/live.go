package state

// Live state shared between copies of obs-beacond (docs/scale-and-
// availability.md, Sharing live state): each host's latest report, and a
// bus of short notices that something changed. Notices carry only names;
// whoever hears one reads the row, so a missed notice costs a delay, not
// data. On SQLite there's one process, so the bus is silent and hosts_live
// isn't written.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

const liveSchema = `
CREATE TABLE IF NOT EXISTS hosts_live (
	host TEXT PRIMARY KEY, report TEXT NOT NULL, received_ms BIGINT NOT NULL
);
`

// Shared says whether this database is shared by several copies of
// obs-beacond (PostgreSQL), so live state goes through it.
func (d *DB) Shared() bool { return d.kind == postgres }

// PutLive stores a host's latest report, unless a newer one is there
// already (two copies can take two reports from one host out of order).
func (d *DB) PutLive(host string, report []byte, received time.Time) error {
	_, err := d.exec(`INSERT INTO hosts_live (host, report, received_ms) VALUES (?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET report = excluded.report, received_ms = excluded.received_ms
		WHERE hosts_live.received_ms < excluded.received_ms`, host, string(report), received.UnixMilli())
	return err
}

// Live reads a host's latest report and when it came.
func (d *DB) Live(host string) ([]byte, time.Time, error) {
	var rep string
	var ms int64
	err := d.queryRow(`SELECT report, received_ms FROM hosts_live WHERE host = ?`, host).Scan(&rep, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	return []byte(rep), time.UnixMilli(ms), err
}

// LiveTimes says when each host's latest report came, to find the ones a
// copy has missed.
func (d *DB) LiveTimes() (map[string]time.Time, error) {
	rows, err := d.query(`SELECT host, received_ms FROM hosts_live`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var h string
		var ms int64
		if err := rows.Scan(&h, &ms); err != nil {
			return nil, err
		}
		out[h] = time.UnixMilli(ms)
	}
	return out, rows.Err()
}

// DeleteLive forgets a host's latest report (the host was removed).
func (d *DB) DeleteLive(host string) error {
	_, err := d.exec(`DELETE FROM hosts_live WHERE host = ?`, host)
	return err
}

// SessionIDs lists the stored sessions (their cookies' SHA-256), so a copy
// can drop the ones another copy ended.
func (d *DB) SessionIDs() (map[string]bool, error) {
	rows, err := d.query(`SELECT token_sha256 FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Session reads one stored session, for a copy that hasn't seen it yet.
func (d *DB) Session(tokenSHA256 string, now time.Time) (Session, error) {
	var s Session
	var exp, chk int64
	err := d.queryRow(`SELECT token_sha256, "user", email, role, provider, dev, expires, checked, sealed FROM sessions WHERE token_sha256 = ? AND expires > ?`,
		tokenSHA256, now.Unix()).Scan(&s.TokenSHA256, &s.User, &s.Email, &s.Role, &s.Provider, &s.Dev, &exp, &chk, &s.Sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	s.Expires, s.Checked = time.Unix(exp, 0), time.Unix(chk, 0)
	return s, err
}

// busChannel is the PostgreSQL channel the copies talk on. Channels are
// database-wide, so it's named after the schema: two installs sharing a
// database in two schemas don't hear each other.
func busChannel(schema string) string {
	c := "cairn_obs_" + schema
	if len(c) > 63 { // the longest identifier PostgreSQL keeps
		c = "cairn_obs_" + hashKey(schema)[:40]
	}
	return c
}

// Notify tells every other copy listening that something changed. It does
// nothing on SQLite.
func (d *DB) Notify(payload string) error {
	if d.kind != postgres {
		return nil
	}
	_, err := d.exec(`SELECT pg_notify(?, ?)`, d.channel, payload)
	return err
}

// Listen hears notices until ctx ends, on a connection of its own. Each time
// it (re)connects it calls ready first: notices sent while it wasn't
// listening are lost, so that's when to catch up. On SQLite it returns at
// once.
func (d *DB) Listen(ctx context.Context, ready func(), heard func(payload string)) {
	if d.kind != postgres {
		return
	}
	wait := time.Second
	for ctx.Err() == nil {
		err := d.listenOnce(ctx, ready, heard, &wait)
		if ctx.Err() != nil {
			return
		}
		log.Printf("state: notices: %v; listening again in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (d *DB) listenOnce(ctx context.Context, ready func(), heard func(string), wait *time.Duration) error {
	conn, err := pgx.ConnectConfig(ctx, d.pg.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{d.channel}.Sanitize()); err != nil {
		return err
	}
	*wait = time.Second
	ready()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		heard(n.Payload)
	}
}
