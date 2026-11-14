package state

// Short-lived sign-in state, kept here rather than in one obs-beacond's
// memory so a sign-in that starts on one copy can finish on another
// (docs/scale-and-availability.md, Sharing live state): sign-ins out at a
// provider, passkey ceremonies, TOTP secrets being set up, and recent
// password failures. Keys are stored only as their SHA-256 and values are
// sealed, so the table hands nobody a sign-in.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

const ceremoniesSchema = `
CREATE TABLE IF NOT EXISTS ceremonies (
	kind TEXT NOT NULL, k TEXT NOT NULL, v TEXT NOT NULL, expires BIGINT NOT NULL,
	PRIMARY KEY (kind, k)
);
CREATE INDEX IF NOT EXISTS ceremonies_expires ON ceremonies (expires);
CREATE TABLE IF NOT EXISTS signin_failures (k TEXT NOT NULL, at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS signin_failures_k ON signin_failures (k, at);
`

func hashKey(k string) string {
	s := sha256.Sum256([]byte(k))
	return hex.EncodeToString(s[:])
}

// PutCeremony keeps v (sealed) under kind and key until expires, replacing
// what was there. Expired ceremonies of every kind go at the same time.
func (d *DB) PutCeremony(kind, key string, v []byte, expires time.Time) error {
	if _, err := d.exec(`DELETE FROM ceremonies WHERE expires <= ?`, time.Now().Unix()); err != nil {
		return err
	}
	_, err := d.exec(`INSERT INTO ceremonies (kind, k, v, expires) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, k) DO UPDATE SET v = excluded.v, expires = excluded.expires`,
		kind, hashKey(key), d.Seal(v), expires.Unix())
	return err
}

// Ceremony reads one that hasn't expired; ErrNotFound otherwise.
func (d *DB) Ceremony(kind, key string, now time.Time) ([]byte, error) {
	var v string
	err := d.queryRow(`SELECT v FROM ceremonies WHERE kind = ? AND k = ? AND expires > ?`, kind, hashKey(key), now.Unix()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.Unseal(v)
}

// TakeCeremony reads one and removes it in the same step, so it's used once
// even when two copies are asked at the same moment.
func (d *DB) TakeCeremony(kind, key string, now time.Time) ([]byte, error) {
	var v string
	var exp int64
	err := d.queryRow(`DELETE FROM ceremonies WHERE kind = ? AND k = ? RETURNING v, expires`, kind, hashKey(key)).Scan(&v, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp <= now.Unix()) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.Unseal(v)
}

// DropCeremony removes one, if it's there.
func (d *DB) DropCeremony(kind, key string) error {
	_, err := d.exec(`DELETE FROM ceremonies WHERE kind = ? AND k = ?`, kind, hashKey(key))
	return err
}

// SigninFailures counts the failures for each key since `since`.
func (d *DB) SigninFailures(since time.Time, keys ...string) (map[string]int, error) {
	out := map[string]int{}
	for _, k := range keys {
		var n int
		if err := d.queryRow(`SELECT count(*) FROM signin_failures WHERE k = ? AND at > ?`, hashKey(k), since.Unix()).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}

// AddSigninFailure records a failure against each key and forgets every
// failure older than `forget`.
func (d *DB) AddSigninFailure(at, forget time.Time, keys ...string) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.Exec(`DELETE FROM signin_failures WHERE at <= ?`, forget.Unix()); err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := t.Exec(`INSERT INTO signin_failures (k, at) VALUES (?, ?)`, hashKey(k), at.Unix()); err != nil {
			return err
		}
	}
	return t.Commit()
}

// ClearSigninFailures forgets a key's failures (after it signs in).
func (d *DB) ClearSigninFailures(key string) error {
	_, err := d.exec(`DELETE FROM signin_failures WHERE k = ?`, hashKey(key))
	return err
}
