package state

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kf := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(kf, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	return kf
}

// Migrations are appended, numbered 1, 2, 3…, and each says what it does
// for both back ends.
func TestMigrationsAreInOrder(t *testing.T) {
	for i, m := range migrations {
		if m.version != i+1 || m.name == "" {
			t.Fatalf("migration %d: version %d, name %q", i, m.version, m.name)
		}
		for _, k := range []dialect{sqlite, postgres} {
			if m.ddl[k] == "" && m.fn == nil {
				t.Fatalf("migration %d does nothing for %s", m.version, k)
			}
		}
	}
}

func TestMigrateRecordsAndRunsOnce(t *testing.T) {
	path, kf := statetest.Database(t, t.TempDir()), testKey(t)
	db, err := Open(path, kf)
	if err != nil {
		t.Fatal(err)
	}
	done, newest, err := db.appliedMigrations()
	if err != nil || newest != len(migrations) || len(done) != len(migrations) {
		t.Fatalf("applied %v, newest %d: %v", done, newest, err)
	}
	db.Close()
	// Opening again runs nothing; a schema a newer obs-beacond moved on is
	// used as it is.
	if db, err = Open(path, kf); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(`INSERT INTO schema_migrations (version, name, applied) VALUES (?, ?, ?)`, len(migrations)+1, "from the future", 0); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(path, kf); err != nil {
		t.Fatalf("a newer schema was refused: %v", err)
	}
	db.Close()
}

func TestDialectRebindsAndPassesFlags(t *testing.T) {
	if q := postgres.rebind(`SELECT a FROM t WHERE b = ? AND c IN (?, ?)`); q != `SELECT a FROM t WHERE b = $1 AND c IN ($2, $3)` {
		t.Fatal(q)
	}
	if q := sqlite.rebind(`SELECT ?`); q != `SELECT ?` {
		t.Fatal(q)
	}
	in := []any{"x", true, false, 3}
	out := postgres.args(in)
	if out[1] != 1 || out[2] != 0 || in[1] != true {
		t.Fatalf("args %v (in %v)", out, in)
	}
}

// Later migrations only expand (migrate.go): nothing a running copy of the
// previous release reads is dropped, renamed or retyped, and nothing takes a
// long lock on a big table. This reads their DDL; a migration's fn is
// reviewed by hand.
func TestMigrationsOnlyExpand(t *testing.T) {
	checkExpandOnly(t, migrations[1:])
	bad := []migration{
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users DROP COLUMN totp`}},
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users RENAME COLUMN name TO full_name`}},
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users ALTER COLUMN created TYPE TEXT`}},
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users ALTER COLUMN totp SET NOT NULL`}},
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users ADD COLUMN x TEXT NOT NULL`}},
		{version: 9, ddl: map[dialect]string{postgres: `CREATE INDEX users_name ON users (name)`}},
		{version: 9, ddl: map[dialect]string{sqlite: `VACUUM`}},
	}
	for _, m := range bad {
		if expandProblem(m) == "" {
			t.Errorf("not caught: %s", m.ddl[postgres]+m.ddl[sqlite])
		}
	}
	ok := []migration{
		{version: 9, ddl: map[dialect]string{postgres: `ALTER TABLE users ADD COLUMN x TEXT NOT NULL DEFAULT ''; ALTER TABLE users ADD COLUMN y TEXT`}},
		{version: 9, ddl: map[dialect]string{postgres: `CREATE INDEX CONCURRENTLY IF NOT EXISTS users_name ON users (name)`, sqlite: `CREATE INDEX IF NOT EXISTS users_name ON users (name)`}},
		{version: 9, ddl: map[dialect]string{postgres: `CREATE TABLE IF NOT EXISTS t2 (a TEXT); CREATE INDEX IF NOT EXISTS t2_a ON t2 (a)`}},
		{version: 9, contract: true, ddl: map[dialect]string{postgres: `ALTER TABLE users DROP COLUMN totp`}},
	}
	for _, m := range ok {
		if p := expandProblem(m); p != "" {
			t.Errorf("refused %q: %s", m.ddl[postgres], p)
		}
	}
}

func checkExpandOnly(t *testing.T, ms []migration) {
	t.Helper()
	for _, m := range ms {
		if p := expandProblem(m); p != "" {
			t.Errorf("migration %d (%s): %s", m.version, m.name, p)
		}
	}
}

var (
	blocking = []struct {
		re       *regexp.Regexp
		why      string
		contract bool // allowed in a contract migration
	}{
		{regexp.MustCompile(`(?i)\bDROP\s+(TABLE|COLUMN|INDEX|CONSTRAINT|SEQUENCE)\b`), "drops something: that's a contract migration, a release after nothing reads it", true},
		{regexp.MustCompile(`(?i)\bRENAME\b`), "renames something the previous release still uses", false},
		{regexp.MustCompile(`(?i)\bALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b`), "changes a column's type: a rewrite under a lock", false},
		{regexp.MustCompile(`(?i)\bSET\s+NOT\s+NULL\b`), "SET NOT NULL scans the whole table under a lock", false},
		{regexp.MustCompile(`(?i)\b(VACUUM|CLUSTER)\b`), "rewrites a table under a lock", false},
	}
	addColumn   = regexp.MustCompile(`(?i)\bADD\s+COLUMN\b[^,;]*`)
	createIndex = regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b(\s+CONCURRENTLY\b)?[^;]*?\bON\s+"?(\w+)`)
	createTable = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?"?(\w+)`)
)

func expandProblem(m migration) string {
	for k, ddl := range m.ddl {
		for _, b := range blocking {
			if b.re.MatchString(ddl) && !(m.contract && b.contract) {
				return fmt.Sprintf("%s DDL %s", k, b.why)
			}
		}
		for _, c := range addColumn.FindAllString(ddl, -1) {
			u := strings.ToUpper(c)
			if strings.Contains(u, "NOT NULL") && !strings.Contains(u, "DEFAULT") {
				return fmt.Sprintf("%s DDL adds a NOT NULL column with no default: the previous release can't insert", k)
			}
		}
		if k == postgres {
			made := map[string]bool{} // an index on a table this migration makes blocks nobody
			for _, t := range createTable.FindAllStringSubmatch(ddl, -1) {
				made[t[2]] = true
			}
			for _, ix := range createIndex.FindAllStringSubmatch(ddl, -1) {
				if ix[2] == "" && !made[ix[3]] {
					return "PostgreSQL DDL builds an index without CONCURRENTLY, blocking writes (and CONCURRENTLY needs the runner to run it outside a transaction: add that with the first one)"
				}
			}
		}
	}
	return ""
}
