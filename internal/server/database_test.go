package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// database takes a SQLite file or a PostgreSQL URL; each refuses what's
// only for the other, and obs-beacond's own files go where they should.
func TestDatabaseSettings(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) (*Config, error) {
		p := filepath.Join(dir, "obs-beacond.yaml")
		_ = os.WriteFile(p, []byte("listen: 127.0.0.1:0\nstate_key_file: /k\n"+extra), 0o600)
		return LoadConfig(p)
	}
	pg := `database: "postgres://obs@db.example.internal/obs?sslmode=verify-full"` + "\n"
	for extra, want := range map[string]string{
		pg + "snapshot: /b/beacon.db\n":                        "snapshot is for SQLite",
		"database: /d/beacon.db\ndatabase_password_file: /p\n": "database_password_file is for a PostgreSQL database",
	} {
		if _, err := load(extra); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", extra, err, want)
		}
	}
	for extra, want := range map[string]string{
		"database: /d/beacon.db\n":               "/d",
		pg:                                       DefaultDataDir,
		pg + "data_dir: /srv/obs-beacon\n":       "/srv/obs-beacon",
		"database: /d/beacon.db\ndata_dir: /e\n": "/e",
		"":                                       "",
	} {
		c, err := load(extra)
		if err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		if c.Dir() != want {
			t.Errorf("%q: files in %q, want %q", extra, c.Dir(), want)
		}
	}
	c, _ := load("database: /d/beacon.db\nagent_listen: \":8461\"\n")
	if f := AgentCAFile(c); f != "/d/agent-ca.pem" {
		t.Errorf("agent CA at %s", f)
	}
}
