package ibmi

import (
	"context"
	"fmt"
	"net/netip"
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
	// Only the subsystems whose descriptions the profile may use are
	// listed; IBM's own usually are public.
	sqlSubsystems = `SELECT SUBSYSTEM_DESCRIPTION_LIBRARY, SUBSYSTEM_DESCRIPTION, STATUS, CURRENT_ACTIVE_JOBS, CONTROLLING_SUBSYSTEM,
TEXT_DESCRIPTION FROM QSYS2.SUBSYSTEM_INFO`
	sqlDisks      = `SELECT ASP_NUMBER, UNIT_NUMBER, RESOURCE_NAME, HARDWARE_STATUS, PROTECTION_STATUS, SSD_PFA_WARNING FROM QSYS2.SYSDISKSTAT`
	sqlInterfaces = `SELECT CONNECTION_TYPE, INTERNET_ADDRESS, PREFIX_LENGTH, LINE_DESCRIPTION, INTERFACE_STATUS, AUTOSTART, INTERFACE_TEXT
FROM QSYS2.NETSTAT_INTERFACE_INFO`
	// QSYSOPR's inquiries and replies of the last 30 days, and its
	// messages of severity 80 and up of the last day. Keys are binary,
	// which Mapepire would hand out as links, so they're read as hex; ages
	// are the partition's own clock's.
	sqlMessages = `SELECT HEX(MESSAGE_KEY) AS MSG_KEY, HEX(ASSOCIATED_MESSAGE_KEY) AS ASSOC_KEY, MESSAGE_ID, MESSAGE_TYPE, MESSAGE_TEXT,
SEVERITY, FROM_JOB, FROM_USER, TIMESTAMPDIFF(2, CHAR(NOW() - MESSAGE_TIMESTAMP)) AS AGE_SECONDS FROM QSYS2.MESSAGE_QUEUE_INFO
WHERE MESSAGE_QUEUE_LIBRARY = 'QSYS' AND MESSAGE_QUEUE_NAME = 'QSYSOPR' AND (
 (MESSAGE_TYPE IN ('INQUIRY', 'REPLY') AND MESSAGE_TIMESTAMP > NOW() - 30 DAYS) OR (SEVERITY >= 80 AND MESSAGE_TIMESTAMP > NOW() - 1 DAY))
ORDER BY MESSAGE_TIMESTAMP DESC FETCH FIRST 4000 ROWS ONLY`
	// Only the queues with jobs ready to run are read: one that's held,
	// or that no active subsystem takes from, keeps them waiting.
	sqlJobQueues = `SELECT JOB_QUEUE_LIBRARY, JOB_QUEUE_NAME, JOB_QUEUE_STATUS, RELEASED_JOBS, SUBSYSTEM_NAME FROM QSYS2.JOB_QUEUE_INFO
WHERE RELEASED_JOBS > 0`
)

// MaxMessages is how many unanswered inquiries, and how many severe
// messages, become records; the rest are counted on the partition's own.
const MaxMessages = 25

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

// Subsystem is one of SUBSYSTEM_INFO.
type Subsystem struct {
	Library, Name string
	Status        string // ACTIVE, ENDING, INACTIVE, RESTRICTED, STARTING
	Jobs          int
	Controlling   bool
	Text          string
}

// Disk is one disk unit of SYSDISKSTAT that isn't fine: only those are
// kept, on their ASP's record.
type Disk struct {
	ASP        int
	Unit       int
	Resource   string // DMP012
	Hardware   string // HARDWARE_STATUS
	Protection string // PROTECTION_STATUS
	PFA        bool   // an SSD's predictive failure warning is logged
}

// Interface is one of NETSTAT_INTERFACE_INFO, loopback and link-local
// left out.
type Interface struct {
	Address   string
	Prefix    int
	Line      string // the line description: ETHLINE
	Status    string // ACTIVE, INACTIVE, STARTING, FAILED, RCYPND, ...
	Autostart bool   // started with TCP/IP: IBM i's own word that it should run
	Text      string
}

// Message is one of QSYSOPR's: an inquiry nobody has answered, or one of
// severity 80 and up.
type Message struct {
	Key      string // hex
	ID       string // CPA0701; empty for an impromptu message
	Inquiry  bool
	Text     string
	Severity int
	Job      string // 123456/BATCH/NIGHTLY
	User     string
	Age      time.Duration
}

// JobQueue is a job queue whose released jobs can't start: it's held, or
// no active subsystem takes from it.
type JobQueue struct {
	Library, Name string
	Held          bool
	Waiting       int    // released jobs on it
	Subsystem     string // the active subsystem that takes from it; empty when none
}

// Snapshot is one read of a partition.
type Snapshot struct {
	Name    string // what it's kept under
	System  System
	CPUPct  *float64 // over the minute since the last read; nil on the first
	Release string   // 7.5
	TR      int      // the Technology Refresh; 0 when it can't be told
	ASPs    []ASP
	Disks   []Disk // the disk units that aren't fine
	Subsys  []Subsystem
	Ifaces  []Interface
	// Inquiries are QSYSOPR's unanswered ones, newest first, and Severe
	// its messages of severity 80 and up of the last day; each list at
	// most MaxMessages long, with the whole count beside it.
	Inquiries   []Message
	Unanswered  int
	Severe      []Message
	SevereCount int
	JobQueues   []JobQueue
	Problems    map[string]string
	At          time.Time
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
	if rows, err := c.query(ctx, sqlDisks); err != nil {
		s.Problems["disk units"] = err.Error()
	} else {
		for _, r := range rows {
			d := Disk{ASP: int(numOr0(r, "ASP_NUMBER")), Unit: int(numOr0(r, "UNIT_NUMBER")), Resource: str(r, "RESOURCE_NAME"),
				Hardware: str(r, "HARDWARE_STATUS"), Protection: str(r, "PROTECTION_STATUS"), PFA: str(r, "SSD_PFA_WARNING") == "YES"}
			if diskTrouble(d) != "" {
				s.Disks = append(s.Disks, d)
			}
		}
		sort.Slice(s.Disks, func(i, j int) bool { return s.Disks[i].Unit < s.Disks[j].Unit })
	}
	if rows, err := c.query(ctx, sqlSubsystems); err != nil {
		s.Problems["subsystems"] = err.Error()
	} else {
		for _, r := range rows {
			s.Subsys = append(s.Subsys, Subsystem{Library: str(r, "SUBSYSTEM_DESCRIPTION_LIBRARY"), Name: str(r, "SUBSYSTEM_DESCRIPTION"),
				Status: str(r, "STATUS"), Jobs: int(numOr0(r, "CURRENT_ACTIVE_JOBS")), Controlling: str(r, "CONTROLLING_SUBSYSTEM") == "YES",
				Text: str(r, "TEXT_DESCRIPTION")})
		}
		sort.Slice(s.Subsys, func(i, j int) bool {
			return s.Subsys[i].Name+s.Subsys[i].Library < s.Subsys[j].Name+s.Subsys[j].Library
		})
	}
	if rows, err := c.query(ctx, sqlInterfaces); err != nil {
		s.Problems["interfaces"] = err.Error()
	} else {
		for _, r := range rows {
			i := Interface{Address: str(r, "INTERNET_ADDRESS"), Prefix: int(numOr0(r, "PREFIX_LENGTH")), Line: str(r, "LINE_DESCRIPTION"),
				Status: str(r, "INTERFACE_STATUS"), Autostart: str(r, "AUTOSTART") == "YES", Text: str(r, "INTERFACE_TEXT")}
			if a, err := netip.ParseAddr(i.Address); err == nil && (a.IsLoopback() || a.IsLinkLocalUnicast()) {
				continue
			}
			s.Ifaces = append(s.Ifaces, i)
		}
		sort.Slice(s.Ifaces, func(i, j int) bool { return s.Ifaces[i].Address < s.Ifaces[j].Address })
	}
	if rows, err := c.query(ctx, sqlMessages); err != nil {
		s.Problems["QSYSOPR"] = err.Error()
	} else {
		readMessages(s, rows)
	}
	if rows, err := c.query(ctx, sqlJobQueues); err != nil {
		s.Problems["job queues"] = err.Error()
	} else {
		for _, r := range rows {
			q := JobQueue{Library: str(r, "JOB_QUEUE_LIBRARY"), Name: str(r, "JOB_QUEUE_NAME"), Held: str(r, "JOB_QUEUE_STATUS") == "HELD",
				Waiting: int(numOr0(r, "RELEASED_JOBS")), Subsystem: str(r, "SUBSYSTEM_NAME")}
			if q.Held || q.Subsystem == "" {
				s.JobQueues = append(s.JobQueues, q)
			}
		}
		sort.Slice(s.JobQueues, func(i, j int) bool {
			return s.JobQueues[i].Name+s.JobQueues[i].Library < s.JobQueues[j].Name+s.JobQueues[j].Library
		})
	}
	return s, nil
}

// readMessages sorts QSYSOPR's rows: an inquiry is answered when a reply
// names its key.
func readMessages(s *Snapshot, rows []mapepire.Row) {
	answered := map[string]bool{}
	for _, r := range rows {
		if str(r, "MESSAGE_TYPE") == "REPLY" {
			answered[str(r, "ASSOC_KEY")] = true
		}
	}
	for _, r := range rows { // newest first
		m := Message{Key: str(r, "MSG_KEY"), ID: str(r, "MESSAGE_ID"), Text: str(r, "MESSAGE_TEXT"), Severity: int(numOr0(r, "SEVERITY")),
			Job: str(r, "FROM_JOB"), User: str(r, "FROM_USER"), Age: time.Duration(numOr0(r, "AGE_SECONDS")) * time.Second}
		switch str(r, "MESSAGE_TYPE") {
		case "REPLY":
		case "INQUIRY":
			if answered[m.Key] {
				continue
			}
			m.Inquiry = true
			s.Unanswered++
			if len(s.Inquiries) < MaxMessages {
				s.Inquiries = append(s.Inquiries, m)
			}
		default:
			if m.Severity < 80 || m.Age > 24*time.Hour {
				continue
			}
			s.SevereCount++
			if len(s.Severe) < MaxMessages {
				s.Severe = append(s.Severe, m)
			}
		}
	}
}

// diskTrouble is what's wrong with a disk unit, "" when nothing: its
// hardware's status, then its protection's (device parity or mirroring),
// then an SSD's own warning that it will fail.
func diskTrouble(d Disk) string {
	switch d.Hardware {
	case "", "ACTIVE", "BUSY":
	case "FAILED":
		return "failed"
	case "DEGRADED":
		return "degraded (a component failed)"
	case "HARDWARE FAILURE":
		return "has a failed redundant component"
	case "NOT READY":
		return "isn't ready"
	case "PARITY REBUILD":
		return "has its parity being rebuilt"
	case "POWER LOSS":
		return "has lost power"
	case "READ WRITE PROTECTED", "WRITE PROTECTED":
		return "is " + strings.ToLower(d.Hardware)
	case "UNPROTECTED":
		return "is unprotected: its parity set lost a unit"
	default:
		return "is " + strings.ToLower(d.Hardware)
	}
	switch d.Protection {
	case "", "ACTIVE", "BUSY":
	case "SUSPEND":
		return "has its mirroring suspended"
	case "RESUME", "RESUME PENDING":
		return "has its mirroring resuming"
	default:
		return "protection " + strings.ToLower(d.Protection)
	}
	if d.PFA {
		return "predicts its own failure (SSD)"
	}
	return ""
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
