package state

// The web interface's hashed files, kept across versions for rolling
// upgrades (docs/scale-and-availability.md, Version skew). While old and
// new copies serve side by side, a page one version sent may ask for a
// script only the other has. Each copy stores its own _app/immutable files
// here when it starts, and serves from here what it doesn't carry itself.
// The names are content hashes, so versions never clash.

import (
	"database/sql"
	"errors"
	"time"
)

// keepVersions is how many versions' files are kept: the newest few to
// have started, which covers a rollout and a rollback.
const keepVersions = 3

func assetsSchema(blob string) string {
	return `
CREATE TABLE IF NOT EXISTS ui_versions (
	version TEXT PRIMARY KEY, started_ms BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS ui_assets (
	version TEXT NOT NULL, path TEXT NOT NULL, body ` + blob + ` NOT NULL,
	PRIMARY KEY (version, path)
);
CREATE INDEX IF NOT EXISTS ui_assets_path ON ui_assets (path);
`
}

// PutAssets records version's files (path → contents) as of now, and drops
// the files of versions older than the newest keepVersions. A rebuilt
// version (a development build keeps its name) replaces its files.
func (d *DB) PutAssets(version string, files map[string][]byte, now time.Time) error {
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.Exec(`INSERT INTO ui_versions (version, started_ms) VALUES (?, ?)
		ON CONFLICT (version) DO UPDATE SET started_ms = excluded.started_ms`, version, now.UnixMilli()); err != nil {
		return err
	}
	have := map[string]bool{}
	rows, err := t.Query(`SELECT path FROM ui_assets WHERE version = ?`, version)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		have[p] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for p, b := range files {
		if have[p] {
			delete(have, p)
			continue
		}
		if _, err := t.Exec(`INSERT INTO ui_assets (version, path, body) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, version, p, b); err != nil {
			return err
		}
	}
	for p := range have { // in the database for this version, but not in this build
		if _, err := t.Exec(`DELETE FROM ui_assets WHERE version = ? AND path = ?`, version, p); err != nil {
			return err
		}
	}
	old := `SELECT version FROM ui_versions WHERE version <> ? ORDER BY started_ms DESC, version LIMIT -1 OFFSET ?`
	if d.kind == postgres {
		old = `SELECT version FROM ui_versions WHERE version <> ? ORDER BY started_ms DESC, version OFFSET ?`
	}
	rows, err = t.Query(old, version, keepVersions-1)
	if err != nil {
		return err
	}
	var drop []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		drop = append(drop, v)
	}
	rows.Close()
	for _, v := range drop {
		if _, err := t.Exec(`DELETE FROM ui_assets WHERE version = ?`, v); err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM ui_versions WHERE version = ?`, v); err != nil {
			return err
		}
	}
	return t.Commit()
}

// Asset is a stored file by its path, from whichever version has it.
func (d *DB) Asset(path string) ([]byte, error) {
	var b []byte
	err := d.queryRow(`SELECT body FROM ui_assets WHERE path = ? LIMIT 1`, path).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
