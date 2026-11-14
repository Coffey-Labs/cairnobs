package state

import (
	"database/sql"
	"encoding/json"
	"time"
)

// History for reports: when things changed state (not every reading, so a
// year of it stays small), an hourly sample per host, every mail round,
// and the notes people add to incidents.
const historySchema = `
CREATE TABLE IF NOT EXISTS status_log (
	entity TEXT NOT NULL, at INTEGER NOT NULL, status TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS status_log_entity ON status_log (entity, at);
CREATE TABLE IF NOT EXISTS host_hours (
	host TEXT NOT NULL, hour INTEGER NOT NULL,
	cpu REAL NOT NULL, cpu_max REAL NOT NULL, mem REAL NOT NULL, mem_max REAL NOT NULL,
	disks TEXT NOT NULL,
	PRIMARY KEY (host, hour)
);
CREATE TABLE IF NOT EXISTS mail_rounds (
	at INTEGER PRIMARY KEY, seconds REAL NOT NULL, queue INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS incident_notes (
	id TEXT PRIMARY KEY, note TEXT NOT NULL, by TEXT NOT NULL, at INTEGER NOT NULL
);
`

// Change is one entity entering a status at a time.
type Change struct {
	Entity string    `json:"entity"`
	At     time.Time `json:"at"`
	Status string    `json:"status"`
}

// LogStatus records that entity entered status at at.
func (d *DB) LogStatus(entity, status string, at time.Time) error {
	_, err := d.exec(`INSERT INTO status_log (entity, at, status) VALUES (?, ?, ?)`, entity, at.Unix(), status)
	return err
}

// LatestStatuses is every entity's last recorded status.
func (d *DB) LatestStatuses() (map[string]string, error) {
	rows, err := d.query(`SELECT entity, status FROM status_log s WHERE at = (SELECT MAX(at) FROM status_log WHERE entity = s.entity)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var e, s string
		if err := rows.Scan(&e, &s); err != nil {
			return nil, err
		}
		out[e] = s
	}
	return out, rows.Err()
}

// StatusChanges is every change in [from, to), plus each entity's last
// change before from (its status as the period began), oldest first.
func (d *DB) StatusChanges(from, to time.Time) ([]Change, error) {
	rows, err := d.query(`
		SELECT entity, at, status, `+d.kind.rowOrder()+` AS n FROM status_log s
			WHERE at < ? AND at = (SELECT MAX(at) FROM status_log WHERE entity = s.entity AND at < ?)
		UNION ALL
		SELECT entity, at, status, `+d.kind.rowOrder()+` AS n FROM status_log WHERE at >= ? AND at < ?
		ORDER BY at, n`, from.Unix(), from.Unix(), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at, n int64
		if err := rows.Scan(&c.Entity, &at, &c.Status, &n); err != nil {
			return nil, err
		}
		c.At = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// HistoryStart is when the first status was recorded (zero: none yet).
func (d *DB) HistoryStart() time.Time {
	var at sql.NullInt64
	_ = d.queryRow(`SELECT MIN(at) FROM status_log`).Scan(&at)
	return fromUnix(at)
}

// DiskSample is one filesystem's use at the end of an hour.
type DiskSample struct {
	Mount string `json:"mount"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// HostHour is one host's hour: CPU and memory averages and peaks (percent),
// and its disks at the end of the hour.
type HostHour struct {
	Host   string       `json:"host"`
	Hour   time.Time    `json:"hour"`
	CPU    float64      `json:"cpu"`
	CPUMax float64      `json:"cpu_max"`
	Mem    float64      `json:"mem"`
	MemMax float64      `json:"mem_max"`
	Disks  []DiskSample `json:"disks"`
}

// PutHostHour stores (or replaces) one host's hour.
func (d *DB) PutHostHour(h HostHour) error {
	b, _ := json.Marshal(h.Disks)
	_, err := d.exec(`INSERT INTO host_hours (host, hour, cpu, cpu_max, mem, mem_max, disks) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host, hour) DO UPDATE SET cpu=excluded.cpu, cpu_max=excluded.cpu_max, mem=excluded.mem, mem_max=excluded.mem_max, disks=excluded.disks`,
		h.Host, h.Hour.Unix(), h.CPU, h.CPUMax, h.Mem, h.MemMax, string(b))
	return err
}

// HostHours is every host's hours in [from, to), oldest first.
func (d *DB) HostHours(from, to time.Time) ([]HostHour, error) {
	rows, err := d.query(`SELECT host, hour, cpu, cpu_max, mem, mem_max, disks FROM host_hours WHERE hour >= ? AND hour < ? ORDER BY hour, host`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostHour
	for rows.Next() {
		var h HostHour
		var hour int64
		var disks string
		if err := rows.Scan(&h.Host, &hour, &h.CPU, &h.CPUMax, &h.Mem, &h.MemMax, &disks); err != nil {
			return nil, err
		}
		h.Hour = time.Unix(hour, 0)
		_ = json.Unmarshal([]byte(disks), &h.Disks)
		out = append(out, h)
	}
	return out, rows.Err()
}

// MailRound is one mail monitor round: Seconds is -1 when nothing arrived,
// Queue -1 when the queue couldn't be read.
type MailRound struct {
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
	Queue   int       `json:"queue"`
}

// PutMailRound stores one round.
func (d *DB) PutMailRound(m MailRound) error {
	_, err := d.exec(`INSERT INTO mail_rounds (at, seconds, queue) VALUES (?, ?, ?)
		ON CONFLICT (at) DO UPDATE SET seconds=excluded.seconds, queue=excluded.queue`, m.At.Unix(), m.Seconds, m.Queue)
	return err
}

// MailRounds is every round in [from, to), oldest first.
func (d *DB) MailRounds(from, to time.Time) ([]MailRound, error) {
	rows, err := d.query(`SELECT at, seconds, queue FROM mail_rounds WHERE at >= ? AND at < ? ORDER BY at`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailRound
	for rows.Next() {
		var m MailRound
		var at int64
		if err := rows.Scan(&at, &m.Seconds, &m.Queue); err != nil {
			return nil, err
		}
		m.At = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// IncidentsBetween is every incident opened in [from, to), oldest first.
// Ones resolved before anyone was told were deleted and aren't here.
func (d *DB) IncidentsBetween(from, to time.Time) ([]Incident, error) {
	rows, err := d.query(`SELECT `+incidentCols+` FROM incidents WHERE opened >= ? AND opened < ? ORDER BY opened`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIncidents(rows)
}

// Note is what someone wrote about an incident after the fact: the cause.
type Note struct {
	Text string    `json:"text"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

// SetNote writes (or with an empty text, removes) an incident's note.
func (d *DB) SetNote(id string, n Note) error {
	if n.Text == "" {
		_, err := d.exec(`DELETE FROM incident_notes WHERE id = ?`, id)
		return err
	}
	_, err := d.exec(`INSERT INTO incident_notes (id, note, "by", at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET note=excluded.note, "by"=excluded."by", at=excluded.at`, id, n.Text, n.By, n.At.Unix())
	return err
}

// Notes are the notes on the given incidents, by id.
func (d *DB) Notes(ids []string) (map[string]Note, error) {
	out := map[string]Note{}
	for _, id := range ids {
		var n Note
		var at int64
		err := d.queryRow(`SELECT note, "by", at FROM incident_notes WHERE id = ?`, id).Scan(&n.Text, &n.By, &at)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		n.At = time.Unix(at, 0)
		out[id] = n
	}
	return out, nil
}

// Prune drops history older than before (status changes keep each
// entity's last one, so its status stays known). PostgreSQL drops whole
// months instead (postgres.go).
func (d *DB) Prune(before time.Time) error {
	if d.kind == postgres {
		return d.prunePostgres(before)
	}
	b := before.Unix()
	for _, q := range []string{
		`DELETE FROM status_log WHERE at < ? AND at < (SELECT MAX(at) FROM status_log s WHERE s.entity = status_log.entity)`,
		`DELETE FROM host_hours WHERE hour < ?`,
		`DELETE FROM workload_hours WHERE hour < ?`,
		`DELETE FROM mail_rounds WHERE at < ?`,
	} {
		if _, err := d.exec(q, b); err != nil {
			return err
		}
	}
	return nil
}
