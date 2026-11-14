// Package statetest gives tests a database on whichever back end the run is
// for. By default it's a SQLite file; with CAIRN_OBS_TEST_POSTGRES set to a
// PostgreSQL URL every test gets a schema of its own on that server, dropped
// when the test ends. CI runs the tests both ways.
package statetest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Env names the PostgreSQL server tests run against.
const Env = "CAIRN_OBS_TEST_POSTGRES"

// Postgres says whether this run is against PostgreSQL.
func Postgres() bool { return os.Getenv(Env) != "" }

var (
	mu      sync.Mutex
	schemas = map[string]string{} // dir → database, so a restart finds its data
)

// Database returns a database setting for a test: a SQLite file in dir, or
// a fresh PostgreSQL schema. Asked again for the same dir, it gives the same
// one, as a restarted server would find its file.
func Database(t testing.TB, dir string) string {
	t.Helper()
	base := os.Getenv(Env)
	if base == "" {
		return filepath.Join(dir, "beacon.db")
	}
	mu.Lock()
	defer mu.Unlock()
	if d, ok := schemas[dir]; ok {
		return d
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		db.Close()
		t.Fatalf("%s: %v", Env, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		db.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	schemas[dir] = u.String()
	t.Cleanup(func() {
		mu.Lock()
		delete(schemas, dir)
		mu.Unlock()
	})
	return u.String()
}

// SQLiteOnly skips a test that's about SQLite itself.
func SQLiteOnly(t testing.TB) {
	t.Helper()
	if Postgres() {
		t.Skip("SQLite only")
	}
}
