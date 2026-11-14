package state

import (
	"database/sql"
	"errors"
	"time"
)

// Settings edited in the UI: each section is
// kept whole, as the same YAML its file would hold, so export and import
// are the stored text. A section set in a config file never reaches here.
const settingsSchema = `
CREATE TABLE IF NOT EXISTS settings (
	section TEXT PRIMARY KEY,
	yaml TEXT NOT NULL,
	updated INTEGER NOT NULL,
	updated_by TEXT NOT NULL
);
`

// Setting is one stored section.
type Setting struct {
	YAML      string
	Updated   time.Time
	UpdatedBy string
}

// Setting returns a stored section; ok is false when it was never saved.
func (d *DB) Setting(section string) (s Setting, ok bool, err error) {
	var at int64
	err = d.queryRow(`SELECT yaml, updated, updated_by FROM settings WHERE section = ?`, section).Scan(&s.YAML, &at, &s.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	s.Updated = time.Unix(at, 0)
	return s, true, nil
}

// PutSetting stores a section, replacing what was there.
func (d *DB) PutSetting(section string, s Setting) error {
	_, err := d.exec(`INSERT INTO settings (section, yaml, updated, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (section) DO UPDATE SET yaml = excluded.yaml, updated = excluded.updated, updated_by = excluded.updated_by`,
		section, s.YAML, s.Updated.Unix(), s.UpdatedBy)
	return err
}
