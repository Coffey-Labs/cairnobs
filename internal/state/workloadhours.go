package state

import (
	"database/sql"
	"errors"
	"time"
)

// What clusters' nodes, controllers and VMs use (docs/cluster-watcher.md,
// step 5): the latest reading per cluster agent, one row each rewritten
// every minute so every copy shows it, and an hourly sample per workload
// for history, kept like host_hours.
func workloadHoursSchema(d dialect) string {
	num, partition, def := "REAL", "", ""
	if d == postgres {
		num = "DOUBLE PRECISION"
		partition = " PARTITION BY RANGE (hour)"
		def = "\nCREATE TABLE IF NOT EXISTS workload_hours_default PARTITION OF workload_hours DEFAULT;"
	}
	return `
CREATE TABLE IF NOT EXISTS workload_usage (
	source TEXT PRIMARY KEY, at_ms BIGINT NOT NULL, usage TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workload_hours (
	source TEXT NOT NULL, workload TEXT NOT NULL, hour BIGINT NOT NULL, kind TEXT NOT NULL,
	cpu ` + num + ` NOT NULL, cpu_max ` + num + ` NOT NULL, cpu_of ` + num + ` NOT NULL,
	mem ` + num + ` NOT NULL, mem_max ` + num + ` NOT NULL, mem_of ` + num + ` NOT NULL,
	PRIMARY KEY (source, workload, hour)
)` + partition + `;` + def + `
`
}

// ioColumns are an array's hour (docs/pure.md, step 4): reads and writes
// a second, microseconds a read or write took (the average, and the
// worst minute's), bytes a second, and space used of the array's
// capacity. Every other workload's are 0.
var ioColumns = []string{"read_ops", "write_ops", "read_us", "read_us_max", "write_us", "write_us_max", "read_bps", "write_bps", "used", "used_of"}

// addIOColumns (migration 15) adds ioColumns to workload_hours; on
// PostgreSQL the partitions get them from the parent.
func addIOColumns(t txn) error {
	num := "REAL"
	if t.kind == postgres {
		num = "DOUBLE PRECISION"
	}
	for _, c := range ioColumns {
		if err := addColumnIfMissing(t, "workload_hours", c, num+" NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	return nil
}

// addLeaseColumns (migration 16) adds a Vault cluster's lease count to
// workload_hours (docs/vault.md, step 3): the hour's average and peak.
func addLeaseColumns(t txn) error {
	num := "REAL"
	if t.kind == postgres {
		num = "DOUBLE PRECISION"
	}
	for _, c := range []string{"leases", "leases_max"} {
		if err := addColumnIfMissing(t, "workload_hours", c, num+" NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	return nil
}

// PutWorkloadUsage keeps a cluster agent's latest reading (usage is its
// JSON), unless a newer one is already there.
func (d *DB) PutWorkloadUsage(source string, at time.Time, usage []byte) error {
	_, err := d.exec(`INSERT INTO workload_usage (source, at_ms, usage) VALUES (?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET at_ms=excluded.at_ms, usage=excluded.usage WHERE workload_usage.at_ms < excluded.at_ms`,
		source, at.UnixMilli(), string(usage))
	return err
}

// WorkloadUsageTimes is when each cluster agent's latest reading was taken:
// cheap, so a copy reads a reading only when it's new.
func (d *DB) WorkloadUsageTimes() (map[string]time.Time, error) {
	rows, err := d.query(`SELECT source, at_ms FROM workload_usage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var s string
		var ms int64
		if err := rows.Scan(&s, &ms); err != nil {
			return nil, err
		}
		out[s] = time.UnixMilli(ms)
	}
	return out, rows.Err()
}

// WorkloadUsage is one cluster agent's latest reading, as sent.
func (d *DB) WorkloadUsage(source string) (time.Time, []byte, error) {
	var ms int64
	var u string
	err := d.queryRow(`SELECT at_ms, usage FROM workload_usage WHERE source = ?`, source).Scan(&ms, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil, nil
	}
	return time.UnixMilli(ms), []byte(u), err
}

// WorkloadHour is one workload's hour: average and peak CPU (cores) and
// memory (bytes), and what it could use (0: no ceiling). Workload is
// "namespace/name" ("shop/deployment/web", "vms/db"), or a node's name, so
// it outlives the object: a Deployment made again keeps its history.
type WorkloadHour struct {
	Source   string    `json:"source"`
	Workload string    `json:"workload"`
	Hour     time.Time `json:"hour"`
	Kind     string    `json:"kind"`
	CPU      float64   `json:"cpu"`
	CPUMax   float64   `json:"cpu_max"`
	CPUOf    float64   `json:"cpu_of,omitempty"`
	Mem      float64   `json:"mem"`
	MemMax   float64   `json:"mem_max"`
	MemOf    float64   `json:"mem_of,omitempty"`
	// An array's (ioColumns): 0 for anything else.
	ReadOps    float64 `json:"read_ops,omitempty"`
	WriteOps   float64 `json:"write_ops,omitempty"`
	ReadUs     float64 `json:"read_us,omitempty"`
	ReadUsMax  float64 `json:"read_us_max,omitempty"`
	WriteUs    float64 `json:"write_us,omitempty"`
	WriteUsMax float64 `json:"write_us_max,omitempty"`
	ReadBps    float64 `json:"read_bps,omitempty"`
	WriteBps   float64 `json:"write_bps,omitempty"`
	Used       float64 `json:"used,omitempty"`
	UsedOf     float64 `json:"used_of,omitempty"`
	// A Vault cluster's (addLeaseColumns).
	Leases    float64 `json:"leases,omitempty"`
	LeasesMax float64 `json:"leases_max,omitempty"`
}

// PutWorkloadHours stores hours; every copy writes the same ones, and the
// last write wins.
func (d *DB) PutWorkloadHours(hs []WorkloadHour) error {
	if len(hs) == 0 {
		return nil
	}
	t, err := d.begin()
	if err != nil {
		return err
	}
	defer t.Rollback()
	for _, h := range hs {
		if _, err := t.Exec(`INSERT INTO workload_hours (source, workload, hour, kind, cpu, cpu_max, cpu_of, mem, mem_max, mem_of,
				read_ops, write_ops, read_us, read_us_max, write_us, write_us_max, read_bps, write_bps, used, used_of, leases, leases_max)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (source, workload, hour) DO UPDATE SET kind=excluded.kind, cpu=excluded.cpu, cpu_max=excluded.cpu_max,
				cpu_of=excluded.cpu_of, mem=excluded.mem, mem_max=excluded.mem_max, mem_of=excluded.mem_of,
				read_ops=excluded.read_ops, write_ops=excluded.write_ops, read_us=excluded.read_us, read_us_max=excluded.read_us_max,
				write_us=excluded.write_us, write_us_max=excluded.write_us_max, read_bps=excluded.read_bps, write_bps=excluded.write_bps,
				used=excluded.used, used_of=excluded.used_of, leases=excluded.leases, leases_max=excluded.leases_max`,
			h.Source, h.Workload, h.Hour.Unix(), h.Kind, h.CPU, h.CPUMax, h.CPUOf, h.Mem, h.MemMax, h.MemOf,
			h.ReadOps, h.WriteOps, h.ReadUs, h.ReadUsMax, h.WriteUs, h.WriteUsMax, h.ReadBps, h.WriteBps, h.Used, h.UsedOf, h.Leases, h.LeasesMax); err != nil {
			return err
		}
	}
	return t.Commit()
}

// WorkloadHours is one workload's hours in [from, to), oldest first.
func (d *DB) WorkloadHours(source, workload string, from, to time.Time) ([]WorkloadHour, error) {
	rows, err := d.query(`SELECT source, workload, hour, kind, cpu, cpu_max, cpu_of, mem, mem_max, mem_of,
		read_ops, write_ops, read_us, read_us_max, write_us, write_us_max, read_bps, write_bps, used, used_of, leases, leases_max FROM workload_hours
		WHERE source = ? AND workload = ? AND hour >= ? AND hour < ? ORDER BY hour`, source, workload, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkloadHour
	for rows.Next() {
		var h WorkloadHour
		var hour int64
		if err := rows.Scan(&h.Source, &h.Workload, &hour, &h.Kind, &h.CPU, &h.CPUMax, &h.CPUOf, &h.Mem, &h.MemMax, &h.MemOf,
			&h.ReadOps, &h.WriteOps, &h.ReadUs, &h.ReadUsMax, &h.WriteUs, &h.WriteUsMax, &h.ReadBps, &h.WriteBps, &h.Used, &h.UsedOf, &h.Leases, &h.LeasesMax); err != nil {
			return nil, err
		}
		h.Hour = time.Unix(hour, 0)
		out = append(out, h)
	}
	return out, rows.Err()
}
