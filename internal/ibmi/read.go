package ibmi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapepire"
)

// The reads: views only, as Mapepire's basic query mode allows
// (docs/ibmi.md, decision 1), each named for what IBM i calls it.
const (
	sqlStatus = `SELECT HOST_NAME, PARTITION_ID, PARTITION_NAME, NUMBER_OF_PARTITIONS, ELAPSED_TIME, ELAPSED_CPU_USED,
CONFIGURED_CPUS, CURRENT_CPU_CAPACITY, MAIN_STORAGE_SIZE, SYSTEM_ASP_STORAGE, SYSTEM_ASP_USED, CURRENT_TEMPORARY_STORAGE,
TOTAL_JOBS_IN_SYSTEM, MAXIMUM_JOBS_IN_SYSTEM, ACTIVE_JOBS_IN_SYSTEM, ACTIVE_THREADS_IN_SYSTEM, RESTRICTED_STATE,
MACHINE_TYPE, MACHINE_MODEL, SERIAL_NUMBER, ATTENTION_LIGHT FROM QSYS2.SYSTEM_STATUS_INFO_BASIC`
	sqlRelease = `SELECT OS_VERSION, OS_RELEASE FROM SYSIBMADM.ENV_SYS_INFO`
	// The Technology Refresh is the level of the TR group installed
	// (SF99967 level 1 is 7.6 TR1). Its description is IBM's; a group
	// that bundles the TR with others says more than that.
	sqlTR = `SELECT PTF_GROUP_NAME, PTF_GROUP_DESCRIPTION, MAX(PTF_GROUP_LEVEL) AS LEVEL FROM QSYS2.GROUP_PTF_INFO
WHERE PTF_GROUP_DESCRIPTION LIKE '%TECHNOLOGY REFRESH%' AND PTF_GROUP_STATUS = 'INSTALLED'
GROUP BY PTF_GROUP_NAME, PTF_GROUP_DESCRIPTION`
	sqlASPs = `SELECT ASP_NUMBER, DEVICE_DESCRIPTION_NAME, RESOURCE_NAME, ASP_STATE, ASP_TYPE, NUMBER_OF_DISK_UNITS, DISK_UNITS_PRESENT,
TOTAL_CAPACITY, TOTAL_CAPACITY_AVAILABLE, STORAGE_THRESHOLD_PERCENTAGE, OVERFLOW_STORAGE FROM QSYS2.ASP_INFO`
)

// System is SYSTEM_STATUS_INFO_BASIC's one row: the partition now.
type System struct {
	HostName      string
	PartitionName string // as the HMC knows it; empty without *USE on QPMLPMGT
	PartitionID   int
	Partitions    int // on the server it runs on
	MachineType   string
	MachineModel  string
	Serial        string
	CPUs          int     // virtual processors
	Capacity      float64 // processing units it may use now
	MainStorageKB int64
	SystemASPMB   int64 // millions of bytes
	SystemASPUsed float64
	TempMB        int64
	Jobs          int
	MaxJobs       int
	ActiveJobs    int
	Threads       int
	Restricted    bool
	Attention     bool // the control panel shows an attention SRC
}

// ASP is one auxiliary storage pool, from ASP_INFO.
type ASP struct {
	Number     int
	Name       string // an independent ASP's device description
	State      string // VARIED ON, VARIED OFF, ACTIVE, AVAILABLE, FAILED, NONE
	Type       string // SYSTEM, USER, PRIMARY, SECONDARY, UDFS
	Units      int
	Present    string // ALL, SOME, NONE
	TotalMB    int64  // 0 when it can't be told
	FreeMB     int64
	Threshold  int // % used at which IBM i warns QSYSOPR
	OverflowMB int64
}

// Snapshot is one read of a partition.
type Snapshot struct {
	Name     string // what it's kept under
	System   System
	CPUPct   *float64 // over the minute since the last read; nil on the first
	Release  string   // 7.5
	TR       int      // the Technology Refresh; 0 when it can't be told
	ASPs     []ASP
	Problems map[string]string
	At       time.Time
}

// cpuMark is a read's ELAPSED_ columns: the CPU used on average since the
// connection's first read, and how long that is.
type cpuMark struct{ elapsed, used float64 }

// Read reads the partition once. Its status is what it's for: without it
// the read failed; anything else that can't be read is a problem on the
// partition's own record.
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Name: c.cfg.Name, Problems: map[string]string{}, At: now}
	rows, err := c.query(ctx, sqlStatus)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ibmi: SYSTEM_STATUS_INFO_BASIC gave no row")
	}
	r := rows[0]
	s.System = System{
		HostName: str(r, "HOST_NAME"), PartitionName: str(r, "PARTITION_NAME"), PartitionID: int(numOr0(r, "PARTITION_ID")),
		Partitions: int(numOr0(r, "NUMBER_OF_PARTITIONS")), MachineType: str(r, "MACHINE_TYPE"), MachineModel: str(r, "MACHINE_MODEL"),
		Serial: str(r, "SERIAL_NUMBER"), CPUs: int(numOr0(r, "CONFIGURED_CPUS")), Capacity: numOr0(r, "CURRENT_CPU_CAPACITY"),
		MainStorageKB: int64(numOr0(r, "MAIN_STORAGE_SIZE")), SystemASPMB: int64(numOr0(r, "SYSTEM_ASP_STORAGE")),
		SystemASPUsed: numOr0(r, "SYSTEM_ASP_USED"), TempMB: int64(numOr0(r, "CURRENT_TEMPORARY_STORAGE")),
		Jobs: int(numOr0(r, "TOTAL_JOBS_IN_SYSTEM")), MaxJobs: int(numOr0(r, "MAXIMUM_JOBS_IN_SYSTEM")),
		ActiveJobs: int(numOr0(r, "ACTIVE_JOBS_IN_SYSTEM")), Threads: int(numOr0(r, "ACTIVE_THREADS_IN_SYSTEM")),
		Restricted: str(r, "RESTRICTED_STATE") == "YES", Attention: str(r, "ATTENTION_LIGHT") == "ON",
	}
	e, eok := num(r, "ELAPSED_TIME")
	u, uok := num(r, "ELAPSED_CPU_USED")
	if eok && uok {
		mark := &cpuMark{e, u}
		s.CPUPct = cpuBetween(c.last, mark)
		c.last = mark
	}

	if rows, err := c.query(ctx, sqlRelease); err != nil {
		s.Problems["release"] = err.Error()
	} else if len(rows) > 0 {
		v, rel := str(rows[0], "OS_VERSION"), str(rows[0], "OS_RELEASE")
		if v != "" && rel != "" {
			s.Release = v + "." + rel
		}
	}
	// Reading the TR needs *USE on WRKPTFGRP, which a shop may not give:
	// not knowing it isn't trouble with the partition.
	if rows, err := c.query(ctx, sqlTR); err == nil {
		s.TR = pickTR(rows)
	}

	if rows, err := c.query(ctx, sqlASPs); err != nil {
		s.Problems["ASPs"] = err.Error()
	} else {
		for _, r := range rows {
			a := ASP{Number: int(numOr0(r, "ASP_NUMBER")), Name: str(r, "DEVICE_DESCRIPTION_NAME"), State: str(r, "ASP_STATE"),
				Type: str(r, "ASP_TYPE"), Units: int(numOr0(r, "NUMBER_OF_DISK_UNITS")), Present: str(r, "DISK_UNITS_PRESENT"),
				Threshold: int(numOr0(r, "STORAGE_THRESHOLD_PERCENTAGE")), OverflowMB: int64(numOr0(r, "OVERFLOW_STORAGE"))}
			if a.Name == "" {
				a.Name = str(r, "RESOURCE_NAME")
			}
			// -2 is "too big to say"; null, "can't be told".
			if t, ok := num(r, "TOTAL_CAPACITY"); ok && t > 0 {
				if f, ok := num(r, "TOTAL_CAPACITY_AVAILABLE"); ok && f >= 0 {
					a.TotalMB, a.FreeMB = int64(t), int64(f)
				}
			}
			s.ASPs = append(s.ASPs, a)
		}
		sort.Slice(s.ASPs, func(i, j int) bool { return s.ASPs[i].Number < s.ASPs[j].Number })
	}
	return s, nil
}

// cpuBetween is the CPU used between two reads of the same connection:
// each averages since its first read, so the minute between is what the
// later one adds. A new connection (elapsed went back) gives nothing yet.
func cpuBetween(prev, cur *cpuMark) *float64 {
	if prev == nil || cur.elapsed <= prev.elapsed {
		return nil
	}
	p := (cur.used*cur.elapsed - prev.used*prev.elapsed) / (cur.elapsed - prev.elapsed)
	p = round1(min(max(p, 0), 100))
	return &p
}

// pickTR is the Technology Refresh from the TR groups installed: the
// group that's only the TR when there is one, its highest level.
func pickTR(rows []mapepire.Row) int {
	best, bestPure := 0, false
	for _, r := range rows {
		d := strings.ToUpper(str(r, "PTF_GROUP_DESCRIPTION"))
		pure := strings.TrimSpace(d) == "TECHNOLOGY REFRESH"
		lvl := int(numOr0(r, "LEVEL"))
		switch {
		case pure && !bestPure:
			best, bestPure = lvl, true
		case pure == bestPure && lvl > best:
			best = lvl
		}
	}
	return best
}
