// Package state is obs-beacond's own database: sessions, incidents, the audit
// log, settings, users, agents and history. It's one SQLite file (pure Go, so
// obs-beacond stays a single static binary) or, for clusters, PostgreSQL
// (postgres.go). The SQL is written once for both (dialect.go).
//
// Nothing secret is stored in the clear. Session cookies are kept only as
// their SHA-256, and a sign-in provider's tokens are sealed with AES-256-GCM
// under a key file that only Cairn OBS can read.
package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// schema and the other baseline schemas are migration 1: frozen. Later
// changes are new migrations (migrate.go).
const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
	token_sha256 TEXT PRIMARY KEY,
	user TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL,
	provider TEXT NOT NULL, dev INTEGER NOT NULL,
	expires INTEGER NOT NULL, checked INTEGER NOT NULL,
	sealed TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS incidents (
	id TEXT PRIMARY KEY, key TEXT NOT NULL,
	severity TEXT NOT NULL, title TEXT NOT NULL, detail TEXT NOT NULL,
	host TEXT NOT NULL, href TEXT NOT NULL,
	opened INTEGER NOT NULL, fired INTEGER, notified INTEGER, resolved INTEGER,
	ack_by TEXT, ack_at INTEGER
);
CREATE INDEX IF NOT EXISTS incidents_open ON incidents (resolved);
CREATE TABLE IF NOT EXISTS audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at INTEGER NOT NULL, who TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_at ON audit (at);
`

// DB is obs-beacond's database.
type DB struct {
	db      *sql.DB
	kind    dialect
	pg      *pgx.ConnConfig // PostgreSQL: for the connection that listens for notices
	channel string          // PostgreSQL: the notice channel (busChannel)
	aead    cipher.AEAD
}

// Open opens (creating if needed) the database: a SQLite file at a path, or
// a PostgreSQL server by its postgres:// URL. keyFile holds 32 random bytes,
// base64: the key that seals provider tokens.
func Open(database, keyFile string) (*DB, error) {
	return OpenWith(database, keyFile, Options{})
}

// OpenWith is Open with the options a PostgreSQL database takes.
func OpenWith(database, keyFile string, o Options) (*DB, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("state key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("state key %s: want 32 bytes, base64", keyFile)
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	d, err := connect(database, o)
	if err != nil {
		return nil, err
	}
	d.aead = aead
	return d, nil
}

// connect opens the database and brings its schema up to date. It needs no
// key: `store copy` moves sealed values across as they are.
func connect(database string, o Options) (*DB, error) {
	if IsPostgres(database) {
		db, pg, err := openPostgres(database, o)
		if err != nil {
			return nil, fmt.Errorf("state: PostgreSQL: %w", err)
		}
		d := &DB{db: db, kind: postgres, pg: pg}
		var schema string
		if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("state: PostgreSQL: %w", err)
		}
		d.channel = busChannel(schema)
		if err := d.migrate(); err != nil {
			db.Close()
			return nil, fmt.Errorf("state: PostgreSQL: %w", err)
		}
		now := time.Now()
		if err := d.EnsurePartitions(now, now.AddDate(0, partitionsAhead, 0)); err != nil {
			db.Close()
			return nil, fmt.Errorf("state: PostgreSQL: %w", err)
		}
		return d, nil
	}
	path := database
	// WAL so readers never wait on the writer; a busy timeout instead of
	// failing when two writes meet; foreign keys are unused but cheap.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; SQLite serialises anyway
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{db: db, kind: sqlite}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	// SQLite creates files with the default umask; only Cairn OBS reads these.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("state %s: %w", f, err)
		}
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Seal encrypts b for storage.
func (d *DB) Seal(b []byte) string {
	nonce := make([]byte, d.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(d.aead.Seal(nonce, nonce, b, nil))
}

// Unseal reverses Seal.
func (d *DB) Unseal(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) < d.aead.NonceSize() {
		return nil, errors.New("sealed value is damaged")
	}
	n := d.aead.NonceSize()
	return d.aead.Open(nil, raw[:n], raw[n:], nil)
}

func unix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}

// --- sessions ---

// Session is a stored session. Sealed is whatever the provider needs to
// re-check it, already encrypted with Seal.
type Session struct {
	TokenSHA256 string
	User, Email string
	Role        string
	Provider    string
	Dev         bool
	Expires     time.Time
	Checked     time.Time
	Sealed      string
}

// PutSession stores or replaces a session.
func (d *DB) PutSession(s Session) error {
	_, err := d.exec(`INSERT INTO sessions (token_sha256, "user", email, role, provider, dev, expires, checked, sealed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (token_sha256) DO UPDATE SET "user"=excluded."user", email=excluded.email, role=excluded.role,
			provider=excluded.provider, dev=excluded.dev, expires=excluded.expires, checked=excluded.checked, sealed=excluded.sealed`,
		s.TokenSHA256, s.User, s.Email, s.Role, s.Provider, s.Dev, s.Expires.Unix(), s.Checked.Unix(), s.Sealed)
	return err
}

// DeleteSession removes a session.
func (d *DB) DeleteSession(tokenSHA256 string) error {
	_, err := d.exec(`DELETE FROM sessions WHERE token_sha256 = ?`, tokenSHA256)
	return err
}

// Sessions returns every unexpired session and drops the expired ones.
func (d *DB) Sessions(now time.Time) ([]Session, error) {
	if _, err := d.exec(`DELETE FROM sessions WHERE expires <= ?`, now.Unix()); err != nil {
		return nil, err
	}
	rows, err := d.query(`SELECT token_sha256, "user", email, role, provider, dev, expires, checked, sealed FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var exp, chk int64
		if err := rows.Scan(&s.TokenSHA256, &s.User, &s.Email, &s.Role, &s.Provider, &s.Dev, &exp, &chk, &s.Sealed); err != nil {
			return nil, err
		}
		s.Expires, s.Checked = time.Unix(exp, 0), time.Unix(chk, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- incidents ---

// Incident is a stored incident; zero times are NULL.
type Incident struct {
	ID, Key                 string
	Severity, Title, Detail string
	Host, Href              string
	Opened, Fired, Notified time.Time
	Resolved                time.Time
	AckBy                   string
	AckAt                   time.Time
	Grouping                string // root-cause grouping as JSON: what it holds back and why; empty for none
}

// PutIncident stores or replaces an incident. Its acknowledgement is only
// stored with a new incident: after that it changes through AckIncident and
// ClearAck, so an evaluator saving an incident never undoes an
// acknowledgement made meanwhile on another copy.
func (d *DB) PutIncident(i Incident) error { return d.PutIncidentAs(i, nil) }

// PutIncidentAs is PutIncident for the holder of a lease: ErrFenced once the
// lease has passed to someone else. A nil fence writes unconditionally.
func (d *DB) PutIncidentAs(i Incident, f *Fence) error {
	return d.PutIncidentNotices(i, f, nil)
}

// PutIncidentNotices saves an incident and queues the notices about it in
// the outbox, in one transaction: a notice is never lost between the two,
// and a refused save (ErrFenced) queues nothing.
func (d *DB) PutIncidentNotices(i Incident, f *Fence, notices []OutboxItem) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	var ackBy any
	if i.AckBy != "" {
		ackBy = i.AckBy
	}
	args := []any{i.ID, i.Key, i.Severity, i.Title, i.Detail, i.Host, i.Href, i.Opened.Unix(),
		unix(i.Fired), unix(i.Notified), unix(i.Resolved), ackBy, unix(i.AckAt), i.Grouping}
	q := `INSERT INTO incidents (id, key, severity, title, detail, host, href, opened, fired, notified, resolved, ack_by, ack_at, grouping)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if f != nil {
		q = `INSERT INTO incidents (id, key, severity, title, detail, host, href, opened, fired, notified, resolved, ack_by, ack_at, grouping)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE ` + fenced
		args = append(args, f.args()...)
	}
	res, err := t.Exec(q+`
		ON CONFLICT (id) DO UPDATE SET key=excluded.key, severity=excluded.severity, title=excluded.title, detail=excluded.detail,
			host=excluded.host, href=excluded.href, opened=excluded.opened, fired=excluded.fired, notified=excluded.notified,
			resolved=excluded.resolved, grouping=excluded.grouping`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); f != nil && n == 0 {
		return ErrFenced
	}
	if err := putOutbox(t, notices); err != nil {
		return err
	}
	return t.Commit()
}

// AckIncident records who acknowledged an open incident; false when there's
// no open incident with that id.
func (d *DB) AckIncident(id, by string, at time.Time) (bool, error) {
	res, err := d.exec(`UPDATE incidents SET ack_by = ?, ack_at = ? WHERE id = ? AND resolved IS NULL`, by, at.Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearAck drops an incident's acknowledgement (it got worse).
func (d *DB) ClearAck(id string) error {
	_, err := d.exec(`UPDATE incidents SET ack_by = NULL, ack_at = NULL WHERE id = ?`, id)
	return err
}

// Incident reads one incident.
func (d *DB) Incident(id string) (Incident, error) {
	rows, err := d.query(`SELECT `+incidentCols+` FROM incidents WHERE id = ?`, id)
	if err != nil {
		return Incident{}, err
	}
	defer rows.Close()
	got, err := scanIncidents(rows)
	if err != nil {
		return Incident{}, err
	}
	if len(got) == 0 {
		return Incident{}, ErrNotFound
	}
	return got[0], nil
}

// DeleteIncident removes an incident (one that resolved before anyone was told).
func (d *DB) DeleteIncident(id string) error {
	_, err := d.exec(`DELETE FROM incidents WHERE id = ?`, id)
	return err
}

const incidentCols = `id, key, severity, title, detail, host, href, opened, fired, notified, resolved, ack_by, ack_at, grouping`

func scanIncidents(rows *sql.Rows) ([]Incident, error) {
	var out []Incident
	for rows.Next() {
		var i Incident
		var opened int64
		var fired, notified, res, ackAt sql.NullInt64
		var ackBy sql.NullString
		if err := rows.Scan(&i.ID, &i.Key, &i.Severity, &i.Title, &i.Detail, &i.Host, &i.Href, &opened, &fired, &notified, &res, &ackBy, &ackAt, &i.Grouping); err != nil {
			return nil, err
		}
		i.Opened, i.Fired, i.Notified, i.Resolved, i.AckAt = time.Unix(opened, 0), fromUnix(fired), fromUnix(notified), fromUnix(res), fromUnix(ackAt)
		i.AckBy = ackBy.String
		out = append(out, i)
	}
	return out, rows.Err()
}

// Incidents returns the open incidents and the newest `recent` resolved ones.
func (d *DB) Incidents(recent int) (open, resolved []Incident, err error) {
	scan := func(q string, args ...any) ([]Incident, error) {
		rows, err := d.query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanIncidents(rows)
	}
	if open, err = scan(`SELECT ` + incidentCols + ` FROM incidents WHERE resolved IS NULL`); err != nil {
		return nil, nil, err
	}
	resolved, err = scan(`SELECT `+incidentCols+` FROM incidents WHERE resolved IS NOT NULL ORDER BY resolved DESC LIMIT ?`, recent)
	return open, resolved, err
}

// Meta reads a small setting; "" when unset.
func (d *DB) Meta(k string) string {
	var v string
	_ = d.queryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	return v
}

// SetMeta writes a small setting.
func (d *DB) SetMeta(k, v string) error { return d.SetMetaAs(k, v, nil) }

// SetMetaAs is SetMeta for the holder of a lease (see PutIncidentAs).
func (d *DB) SetMetaAs(k, v string, f *Fence) error {
	q, args := `INSERT INTO meta (k, v) VALUES (?, ?)`, []any{k, v}
	if f != nil {
		q, args = `INSERT INTO meta (k, v) SELECT ?, ? WHERE `+fenced, append(args, f.args()...)
	}
	res, err := d.exec(q+` ON CONFLICT (k) DO UPDATE SET v = excluded.v`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); f != nil && n == 0 {
		return ErrFenced
	}
	return nil
}

// --- audit log ---

// Entry is one line of the audit log.
type Entry struct {
	At     time.Time `json:"at"`
	Who    string    `json:"who"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

// Audit appends to the audit log.
func (d *DB) Audit(e Entry) error {
	_, err := d.exec(`INSERT INTO audit (at, who, action, detail) VALUES (?, ?, ?, ?)`, e.At.Unix(), e.Who, e.Action, e.Detail)
	return err
}

// AuditLog returns the newest entries first.
func (d *DB) AuditLog(limit int) ([]Entry, error) {
	rows, err := d.query(`SELECT at, who, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var at int64
		if err := rows.Scan(&at, &e.Who, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Backup writes a consistent copy of the database to path (VACUUM INTO),
// for the nightly backup. SQLite only: PostgreSQL has its own backups.
func (d *DB) Backup(path string) error {
	if d.kind != sqlite {
		return errors.New("snapshots are for SQLite; back PostgreSQL up with its own tools")
	}
	_ = os.Remove(path)
	if _, err := d.exec(`VACUUM INTO ?`, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
