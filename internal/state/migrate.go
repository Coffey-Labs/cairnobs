package state

// Numbered schema migrations (docs/scale-and-availability.md, "Changing the
// schema without stopping"). Each runs once, in its own transaction, and is
// recorded in schema_migrations.
//
// The rules, so old and new obs-beacond can share one database during a
// rolling upgrade:
//   - Migrations are only ever appended. A released one is never edited:
//     the baseline schemas (schema, usersSchema, historySchema, …) are
//     migration 1 and frozen.
//   - Expand, then contract: a migration may add tables, add nullable (or
//     defaulted) columns and add indexes. It may not rename, drop or rewrite
//     anything the previous release still reads; that waits a release.
//   - So a database a newer obs-beacond has migrated still works for this
//     one, which only notes it in the log.

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

type migration struct {
	version int
	name    string
	ddl     map[dialect]string // run first, when set for the dialect
	fn      func(t txn) error  // then this, for what DDL alone can't say
	// contract marks the second half of a removal: the release before
	// stopped using what this one drops. Only then may DDL drop anything.
	contract bool
}

var migrations = []migration{
	{version: 1, name: "baseline",
		ddl: map[dialect]string{
			sqlite:   schema + usersSchema + historySchema + agentsSchema + joinedSchema + settingsSchema,
			postgres: pgSchema,
		}},
	{version: 2, name: "users: emergency door, password and TOTP", fn: addUserColumns},
	{version: 3, name: "sign-ins in progress and failures, shared by every copy",
		ddl: map[dialect]string{sqlite: ceremoniesSchema, postgres: ceremoniesSchema}},
	{version: 4, name: "each host's latest report, shared by every copy",
		ddl: map[dialect]string{sqlite: liveSchema, postgres: liveSchema}},
	{version: 5, name: "leases: one evaluator at a time",
		ddl: map[dialect]string{sqlite: leasesSchema, postgres: leasesSchema}},
	{version: 6, name: "the web interface's files, kept across versions",
		ddl: map[dialect]string{sqlite: assetsSchema("BLOB"), postgres: assetsSchema("BYTEA")}},
	{version: 7, name: "the outbox: notices waiting to be sent",
		ddl: map[dialect]string{sqlite: outboxSchema, postgres: outboxSchema}},
	{version: 8, name: "readings: what the evaluator polls, shared by every copy",
		ddl: map[dialect]string{sqlite: readingsSchema, postgres: readingsSchema}},
	{version: 9, name: "workloads from cluster agents, shared by every copy",
		ddl: map[dialect]string{sqlite: workloadsSchema, postgres: workloadsSchema}},
	{version: 10, name: "joined agents and join tokens for clusters", fn: addClusterColumns},
	{version: 11, name: "map edges: dependencies learned from connections",
		ddl: map[dialect]string{sqlite: mapEdgesSchema, postgres: mapEdgesSchema}},
	{version: 12, name: "map services: when each was first seen",
		ddl: map[dialect]string{sqlite: mapServicesSchema, postgres: mapServicesSchema}},
	{version: 13, name: "incidents keep their root-cause grouping", fn: func(t txn) error {
		return addColumnIfMissing(t, "incidents", "grouping", "TEXT NOT NULL DEFAULT ''")
	}},
	{version: 14, name: "what clusters' workloads use: the latest reading and hourly history",
		ddl: map[dialect]string{sqlite: workloadHoursSchema(sqlite), postgres: workloadHoursSchema(postgres)}},
	{version: 15, name: "workload hours: arrays' performance and space", fn: addIOColumns},
	{version: 16, name: "workload hours: Vault's lease count", fn: addLeaseColumns},
	{version: 17, name: "agent actions: what admins asked agents to do",
		ddl: map[dialect]string{sqlite: actionsSchema, postgres: actionsSchema}},
}

// migrate brings the schema up to date. On PostgreSQL several copies may
// start at once: each migration takes an advisory lock and checks again
// that no other copy ran it meanwhile.
func (d *DB) migrate() error {
	if err := d.locked(func(t txn) error {
		_, err := t.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied BIGINT NOT NULL)`)
		return err
	}); err != nil {
		return err
	}
	done, newest, err := d.appliedMigrations()
	if err != nil {
		return err
	}
	if latest := migrations[len(migrations)-1].version; newest > latest {
		log.Printf("state: the schema is at %d, newer than this obs-beacond's %d: a newer obs-beacond migrated it; carrying on (migrations only add)", newest, latest)
	}
	for _, m := range migrations {
		if done[m.version] {
			continue
		}
		if err := d.locked(func(t txn) error { return apply(t, m) }); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// locked runs fn in a transaction that holds the migration lock (on
// PostgreSQL; SQLite has one writer anyway).
func (d *DB) locked(fn func(t txn) error) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if d.kind == postgres {
		if _, err := t.Exec(`SELECT pg_advisory_xact_lock(?)`, lockMigrate); err != nil {
			return err
		}
	}
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

func (d *DB) appliedMigrations() (map[int]bool, int, error) {
	rows, err := d.query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	done, newest := map[int]bool{}, 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, 0, err
		}
		done[v] = true
		newest = max(newest, v)
	}
	return done, newest, rows.Err()
}

func apply(t txn, m migration) error {
	var ran bool // by another copy, while this one waited for the lock
	if err := t.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)`, m.version).Scan(&ran); err != nil || ran {
		return err
	}
	if ddl := m.ddl[t.kind]; ddl != "" {
		if _, err := t.Exec(ddl); err != nil {
			return err
		}
	}
	if m.fn != nil {
		if err := m.fn(t); err != nil {
			return err
		}
	}
	_, err := t.Exec(`INSERT INTO schema_migrations (version, name, applied) VALUES (?, ?, ?)`, m.version, m.name, time.Now().Unix())
	return err
}

// addUserColumns adds the columns later versions brought to users: the
// emergency door, then passwords with TOTP. A database made before
// migrations were numbered may have some of them already.
func addUserColumns(t txn) error {
	if t.kind != sqlite {
		return nil // PostgreSQL's baseline has them
	}
	rows, err := t.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		has[name] = true
	}
	rows.Close()
	for _, c := range []struct{ name, def string }{
		{"emergency", "INTEGER NOT NULL DEFAULT 0"},
		{"password", "TEXT"}, // Argon2id, PHC string; NULL: passkeys only
		{"totp", "TEXT"},     // sealed base32 secret
		{"totp_step", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !has[c.name] {
			if _, err := t.Exec(`ALTER TABLE users ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

// addColumnIfMissing adds a column unless the table has it already, as it
// does when a migration runs again over a database that had it (a schema
// from before migrations were numbered, or a restored copy). def must
// allow the previous release's inserts: nullable, or with a default.
func addColumnIfMissing(t txn, table, col, def string) error {
	var n int
	var err error
	if t.kind == sqlite {
		err = t.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
	} else {
		err = t.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`, table, col).Scan(&n)
	}
	if err != nil || n > 0 {
		return err
	}
	_, err = t.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}
