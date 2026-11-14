package state

// PostgreSQL, for clusters (docs/scale-and-availability.md, Storage). The
// queries are the same as SQLite's; what's here is how to connect, the
// schema in PostgreSQL's own types, and the partitions that let history grow
// and shrink without DELETE storms.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// IsPostgres says whether a database setting names a PostgreSQL server
// rather than a SQLite file.
func IsPostgres(database string) bool {
	return strings.HasPrefix(database, "postgres://") || strings.HasPrefix(database, "postgresql://")
}

// Options are what a PostgreSQL database takes besides its URL.
type Options struct {
	// PasswordFile holds the password, so it's never in the URL or the
	// config file.
	PasswordFile string
	// Wait is how long to keep trying a database that isn't there yet,
	// as when a cluster starts it and obs-beacond together. A refused
	// password or a missing database fails at once. Waiting tells Log.
	Wait time.Duration
	Log  func(format string, args ...any)
}

func openPostgres(url string, o Options) (*sql.DB, *pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(url) // its errors leave the password out
	if err != nil {
		return nil, nil, err
	}
	if o.PasswordFile != "" {
		b, err := os.ReadFile(o.PasswordFile)
		if err != nil {
			return nil, nil, fmt.Errorf("database password: %w", err)
		}
		cfg.Password = strings.TrimRight(string(b), "\r\n")
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "obs-beacond"
	}
	db := stdlib.OpenDB(*cfg)
	// Enough for a busy copy; connections are recycled so a failover's new
	// primary is picked up even by ones that never broke.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := ping(db, o); err != nil {
		db.Close()
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&v); err != nil {
		db.Close()
		return nil, nil, err
	}
	if v < 160000 {
		db.Close()
		return nil, nil, fmt.Errorf("PostgreSQL %d.%d is too old: Cairn OBS needs 16 or newer", v/10000, v%10000)
	}
	return db, cfg, nil
}

// ping reaches the database, waiting up to o.Wait while it isn't up yet.
func ping(db *sql.DB, o Options) error {
	deadline := time.Now().Add(o.Wait)
	var told time.Time
	for delay := 500 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := db.PingContext(ctx)
		cancel()
		left := time.Until(deadline)
		if err == nil || !notUpYet(err) || left <= 0 {
			return err
		}
		if o.Log != nil && time.Since(told) >= 30*time.Second {
			o.Log("waiting for the database: %v", err)
			told = time.Now()
		}
		time.Sleep(min(delay, left))
	}
}

// notUpYet is true of errors that can pass by themselves: nothing
// listening, no route or name yet, or a server still starting. The server
// answering with anything else (a wrong password, no such database) is
// final.
func notUpYet(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "57P03" // cannot_connect_now: starting up, shutting down, recovering
	}
	return true
}

// Advisory locks, so copies starting together take turns. The numbers are
// arbitrary; they only have to be the same in every copy.
const (
	lockMigrate    = 0x636f6273_0001 // "cobs" 1
	lockPartitions = 0x636f6273_0002
)

// pgSchema is migration 1 for PostgreSQL: the same tables as SQLite's
// baseline, with times as BIGINT (Unix seconds), flags as INTEGER, "user"
// and "by" quoted, and history partitioned by month. Frozen like the rest.
const pgSchema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
	token_sha256 TEXT PRIMARY KEY,
	"user" TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL,
	provider TEXT NOT NULL, dev INTEGER NOT NULL,
	expires BIGINT NOT NULL, checked BIGINT NOT NULL,
	sealed TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS incidents (
	id TEXT PRIMARY KEY, key TEXT NOT NULL,
	severity TEXT NOT NULL, title TEXT NOT NULL, detail TEXT NOT NULL,
	host TEXT NOT NULL, href TEXT NOT NULL,
	opened BIGINT NOT NULL, fired BIGINT, notified BIGINT, resolved BIGINT,
	ack_by TEXT, ack_at BIGINT
);
CREATE INDEX IF NOT EXISTS incidents_open ON incidents (resolved);
CREATE INDEX IF NOT EXISTS incidents_opened ON incidents (opened);

CREATE SEQUENCE IF NOT EXISTS audit_id;
CREATE TABLE IF NOT EXISTS audit (
	id BIGINT NOT NULL DEFAULT nextval('audit_id'),
	at BIGINT NOT NULL, who TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL,
	PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);
CREATE INDEX IF NOT EXISTS audit_at ON audit (at);
CREATE TABLE IF NOT EXISTS audit_default PARTITION OF audit DEFAULT;

CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL,
	created BIGINT NOT NULL, disabled INTEGER NOT NULL DEFAULT 0, last_sign_in BIGINT,
	emergency INTEGER NOT NULL DEFAULT 0,
	password TEXT, totp TEXT, totp_step BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users (email);
CREATE TABLE IF NOT EXISTS passkeys (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL, credential TEXT NOT NULL, created BIGINT NOT NULL, last_used BIGINT
);
CREATE INDEX IF NOT EXISTS passkeys_user ON passkeys (user_id);
CREATE TABLE IF NOT EXISTS setup_links (
	token_sha256 TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires BIGINT NOT NULL
);

CREATE SEQUENCE IF NOT EXISTS status_log_id;
CREATE TABLE IF NOT EXISTS status_log (
	id BIGINT NOT NULL DEFAULT nextval('status_log_id'),
	entity TEXT NOT NULL, at BIGINT NOT NULL, status TEXT NOT NULL
) PARTITION BY RANGE (at);
CREATE INDEX IF NOT EXISTS status_log_entity ON status_log (entity, at);
CREATE TABLE IF NOT EXISTS status_log_default PARTITION OF status_log DEFAULT;
CREATE TABLE IF NOT EXISTS host_hours (
	host TEXT NOT NULL, hour BIGINT NOT NULL,
	cpu DOUBLE PRECISION NOT NULL, cpu_max DOUBLE PRECISION NOT NULL,
	mem DOUBLE PRECISION NOT NULL, mem_max DOUBLE PRECISION NOT NULL,
	disks TEXT NOT NULL,
	PRIMARY KEY (host, hour)
) PARTITION BY RANGE (hour);
CREATE TABLE IF NOT EXISTS host_hours_default PARTITION OF host_hours DEFAULT;
CREATE TABLE IF NOT EXISTS mail_rounds (
	at BIGINT PRIMARY KEY, seconds DOUBLE PRECISION NOT NULL, queue INTEGER NOT NULL
) PARTITION BY RANGE (at);
CREATE TABLE IF NOT EXISTS mail_rounds_default PARTITION OF mail_rounds DEFAULT;
CREATE TABLE IF NOT EXISTS incident_notes (
	id TEXT PRIMARY KEY, note TEXT NOT NULL, "by" TEXT NOT NULL, at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_certs (
	serial TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	issued BIGINT NOT NULL,
	not_after BIGINT NOT NULL,
	revoked BIGINT
);
CREATE INDEX IF NOT EXISTS agent_certs_agent ON agent_certs (agent);
CREATE TABLE IF NOT EXISTS agents (
	name TEXT PRIMARY KEY,
	workstation INTEGER NOT NULL DEFAULT 0,
	added BIGINT NOT NULL,
	added_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS join_tokens (
	token_sha256 TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	workstation INTEGER NOT NULL DEFAULT 0,
	created BIGINT NOT NULL,
	created_by TEXT NOT NULL,
	expires BIGINT NOT NULL,
	used BIGINT
);
CREATE TABLE IF NOT EXISTS settings (
	section TEXT PRIMARY KEY,
	yaml TEXT NOT NULL,
	updated BIGINT NOT NULL,
	updated_by TEXT NOT NULL
);
`

// partitioned are the history tables split by month, and the column that
// splits them. Retention drops whole months of the pruned ones; the audit
// log is kept.
var partitioned = []struct {
	table, col string
	pruned     bool
}{
	{"status_log", "at", true},
	{"host_hours", "hour", true},
	{"workload_hours", "hour", true},
	{"mail_rounds", "at", true},
	{"audit", "at", false},
}

// partitionsAhead is how many months past this one have partitions ready,
// so a month never starts without one even if obs-beacond is down for weeks.
const partitionsAhead = 2

func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func partitionName(table string, month time.Time) string {
	return fmt.Sprintf("%s_%04d%02d", table, month.Year(), int(month.Month()))
}

// partitionMonth reads the month back from a partition's name; false for the
// default partition or anything else.
func partitionMonth(table, name string) (time.Time, bool) {
	s, ok := strings.CutPrefix(name, table+"_")
	if !ok || len(s) != 6 {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(n/100, time.Month(n%100), 1, 0, 0, 0, 0, time.UTC), true
}

// EnsurePartitions makes the monthly partitions from `from`'s month to
// `to`'s, for every partitioned table. Rows that landed in a default
// partition because their month had none yet move into the new one. It does
// nothing on SQLite.
func (d *DB) EnsurePartitions(from, to time.Time) error {
	if d.kind != postgres {
		return nil
	}
	for m := monthOf(from); !m.After(monthOf(to)); m = m.AddDate(0, 1, 0) {
		for _, p := range partitioned {
			if err := d.addPartition(p.table, p.col, m); err != nil {
				return fmt.Errorf("partition %s: %w", partitionName(p.table, m), err)
			}
		}
	}
	return nil
}

func (d *DB) addPartition(table, col string, month time.Time) error {
	name := partitionName(table, month)
	lo, hi := month.Unix(), month.AddDate(0, 1, 0).Unix()
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.Exec(`SELECT pg_advisory_xact_lock(?)`, lockPartitions); err != nil {
		return err
	}
	var exists bool
	if err := t.QueryRow(`SELECT to_regclass(?) IS NOT NULL`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	// A new partition can't overlap rows already in the default one, so
	// those step aside first and come back through the parent.
	// (The bounds are our own numbers, written in: CREATE TABLE AS takes no
	// parameters.)
	def := pgx.Identifier{table + "_default"}.Sanitize()
	in := fmt.Sprintf(`%s >= %d AND %s < %d`, col, lo, col, hi)
	moved, err := t.Exec(`CREATE TEMP TABLE moving ON COMMIT DROP AS SELECT * FROM ` + def + ` WHERE ` + in)
	if err != nil {
		return err
	}
	n, _ := moved.RowsAffected()
	if n > 0 {
		if _, err := t.Exec(`DELETE FROM ` + def + ` WHERE ` + in); err != nil {
			return err
		}
	}
	if _, err := t.Exec(fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM (%d) TO (%d)`,
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{table}.Sanitize(), lo, hi)); err != nil {
		return err
	}
	if n > 0 {
		if _, err := t.Exec(`INSERT INTO ` + pgx.Identifier{table}.Sanitize() + ` SELECT * FROM moving`); err != nil {
			return err
		}
	}
	return t.Commit()
}

// partitions lists a table's monthly partitions, oldest first.
func (d *DB) partitions(table string) ([]time.Time, error) {
	rows, err := d.query(`SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = ? AND p.relnamespace = to_regnamespace(current_schema())
		ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if m, ok := partitionMonth(table, name); ok {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// prunePostgres drops every month of history that ended before `before`,
// whole, and trims what's in the default partitions. Within the newest
// month that's due, rows wait until the month is over: retention is whole
// months, so history is kept up to a month longer than asked.
func (d *DB) prunePostgres(before time.Time) error {
	if err := d.EnsurePartitions(time.Now(), time.Now().AddDate(0, partitionsAhead, 0)); err != nil {
		return err
	}
	for _, p := range partitioned {
		if !p.pruned {
			continue
		}
		months, err := d.partitions(p.table)
		if err != nil {
			return err
		}
		for _, m := range months {
			if m.AddDate(0, 1, 0).After(before) {
				break
			}
			if err := d.dropPartition(p.table, m); err != nil {
				return fmt.Errorf("drop %s: %w", partitionName(p.table, m), err)
			}
		}
	}
	b := before.Unix()
	for _, q := range []string{
		`DELETE FROM status_log_default WHERE at < ? AND at < (SELECT MAX(at) FROM status_log s WHERE s.entity = status_log_default.entity)`,
		`DELETE FROM host_hours_default WHERE hour < ?`,
		`DELETE FROM workload_hours_default WHERE hour < ?`,
		`DELETE FROM mail_rounds_default WHERE at < ?`,
	} {
		if _, err := d.exec(q, b); err != nil {
			return err
		}
	}
	return nil
}

// dropPartition detaches a month and drops it. For status_log, the last
// status of anything that hasn't changed since is kept first (it goes to
// the default partition), as SQLite's prune keeps it: it's what that thing
// still is.
func (d *DB) dropPartition(table string, month time.Time) error {
	name := pgx.Identifier{partitionName(table, month)}.Sanitize()
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.Exec(`SELECT pg_advisory_xact_lock(?)`, lockPartitions); err != nil {
		return err
	}
	if _, err := t.Exec(`ALTER TABLE ` + pgx.Identifier{table}.Sanitize() + ` DETACH PARTITION ` + name); err != nil {
		return err
	}
	if table == "status_log" {
		if _, err := t.Exec(`INSERT INTO status_log (id, entity, at, status)
			SELECT o.id, o.entity, o.at, o.status FROM ` + name + ` o
			WHERE o.at = (SELECT MAX(at) FROM ` + name + ` o2 WHERE o2.entity = o.entity)
			AND NOT EXISTS (SELECT 1 FROM status_log s WHERE s.entity = o.entity AND s.at > o.at)`); err != nil {
			return err
		}
	}
	if _, err := t.Exec(`DROP TABLE ` + name); err != nil {
		return err
	}
	return t.Commit()
}
