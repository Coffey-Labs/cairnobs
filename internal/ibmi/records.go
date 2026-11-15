package ibmi

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Distribution is what a partition's own record says it is.
const Distribution = "IBM i"

// Records makes a snapshot into the records a cluster agent sends: the
// partition itself, and its ASPs, subsystems and interfaces as its parts
// (docs/ibmi.md, decision 3). IBM i judges itself: an ASP warns at its own
// storage threshold, the one that sends QSYSOPR CPF0907 or CPI0953; a
// disk unit by its own status; an interface that's set to start with
// TCP/IP and isn't active. No thresholds of ours. Which subsystems should
// be active is expected.yaml's to say, so obs-beacond judges that.
func Records(s *Snapshot) []report.Workload {
	recs := []report.Workload{self(s)}
	disks := map[int][]Disk{}
	for _, d := range s.Disks {
		disks[d.ASP] = append(disks[d.ASP], d)
	}
	for _, a := range s.ASPs {
		recs = append(recs, aspRecord(a, disks[a.Number]))
	}
	for _, b := range s.Subsys {
		recs = append(recs, subsystemRecord(b))
	}
	for _, i := range s.Ifaces {
		recs = append(recs, interfaceRecord(i))
	}
	seen := map[string]bool{} // one record a name: the newest
	for _, m := range append(append([]Message{}, s.Inquiries...), s.Severe...) {
		r := messageRecord(m, s.At)
		if !seen[r.Name] {
			seen[r.Name] = true
			recs = append(recs, r)
		}
	}
	for _, q := range s.JobQueues {
		recs = append(recs, jobQueueRecord(q))
	}
	return recs
}

// messageRecord is one of QSYSOPR's messages: an unanswered inquiry warns
// (a job in MSGW is waiting on it, docs/ibmi.md decision 2), a message of
// severity 90 and up is critical, 80 to 89 a warning. Its name says what
// and from which job, so two jobs' same question are two records.
func messageRecord(m Message, at time.Time) report.Workload {
	what := m.ID
	if what == "" {
		what = "message"
	}
	if m.Job != "" {
		what += " from " + m.Job
	}
	kind := "message/"
	if m.Inquiry {
		kind = "inquiry/"
	}
	r := report.Workload{Kind: report.KindCluster, ID: kind + m.Key, Name: kind + what, State: report.StateDegraded, Word: short(m.Text, 200),
		Facts: map[string]any{"severity": m.Severity}}
	if !at.IsZero() {
		r.Since = at.Add(-m.Age).Round(time.Minute) // the two clocks differ by seconds: kept still
	}
	if m.ID != "" {
		r.Facts["message_id"] = m.ID
	}
	if m.Job != "" {
		r.Facts["job"] = m.Job
	}
	if m.User != "" {
		r.Facts["user"] = m.User
	}
	if !m.Inquiry && m.Severity >= 90 {
		r.State = report.StateDown
	}
	if r.Word == "" {
		r.Word = "No text"
	}
	return r
}

// jobQueueRecord is a job queue whose ready jobs can't start.
func jobQueueRecord(q JobQueue) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: "jobqueue/" + q.Library + "/" + q.Name, Name: "jobqueue/" + q.Name, State: report.StateDegraded,
		Facts: map[string]any{"library": q.Library, "waiting": q.Waiting}}
	jobs := fmt.Sprintf("%d %s waiting", q.Waiting, map[bool]string{true: "job", false: "jobs"}[q.Waiting == 1])
	switch {
	case q.Held:
		r.Word = "Held, with " + jobs
	default:
		r.Word = upper(jobs) + ", and no active subsystem takes from it"
	}
	return r
}

// subsystemRecord is one subsystem. Inactive is stopped, not trouble:
// whether it should run is expected.yaml's.
func subsystemRecord(b Subsystem) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: "subsystem/" + b.Library + "/" + b.Name, Name: "subsystem/" + b.Name,
		State: report.StateOK, Facts: map[string]any{"library": b.Library, "jobs": b.Jobs}}
	if b.Text != "" {
		r.Facts["text"] = b.Text
	}
	if b.Controlling {
		r.Facts["controlling"] = true
	}
	switch b.Status {
	case "ACTIVE":
		r.Word = fmt.Sprintf("Active, %d %s", b.Jobs, map[bool]string{true: "job", false: "jobs"}[b.Jobs == 1])
	case "STARTING":
		r.State, r.Word = report.StateStarting, "Starting"
	case "ENDING":
		r.State, r.Word = report.StateUpdating, "Ending"
	case "RESTRICTED":
		r.State, r.Word = report.StateStopped, "Restricted: the system is in restricted state"
	default:
		r.State, r.Word = report.StateStopped, "Inactive"
	}
	return r
}

// interfaceRecord is one TCP/IP interface. One that isn't set to start
// with TCP/IP is said, not judged.
func interfaceRecord(i Interface) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: "interface/" + i.Address, Name: "interface/" + i.Address, State: report.StateOK,
		Facts: map[string]any{"autostart": i.Autostart}}
	if i.Prefix > 0 {
		r.Facts["prefix"] = i.Prefix
	}
	if i.Line != "" {
		r.Facts["line"] = i.Line
	}
	if i.Text != "" {
		r.Facts["text"] = i.Text
	}
	on := ""
	if i.Line != "" {
		on = " on " + i.Line
	}
	status := strings.ToLower(i.Status)
	switch {
	case i.Status == "ACTIVE":
		r.Word = "Active" + on
	case i.Status == "STARTING" || i.Status == "ACQUIRING":
		r.State, r.Word = report.StateStarting, upper(status)+on
	case !i.Autostart:
		r.State, r.Word = report.StateStopped, upper(status)+on+" (not set to start with TCP/IP)"
	case i.Status == "RCYPND":
		r.State, r.Word = report.StateDown, "Its line"+on+" is waiting for recovery (RCYPND)"
	case i.Status == "RCYCNL":
		r.State, r.Word = report.StateDown, "Its line"+on+" failed and recovery was cancelled (RCYCNL)"
	case strings.HasPrefix(i.Status, "FAILED"):
		r.State, r.Word = report.StateDown, upper(status)+on
	default:
		r.State, r.Word = report.StateDegraded, upper(status)+on+", though it's set to start with TCP/IP"
	}
	return r
}

func self(s *Snapshot) report.Workload {
	y := s.System
	name := y.PartitionName
	if name == "" {
		name = strings.ToLower(y.HostName)
	}
	if name == "" {
		name = s.Name
	}
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "Answering",
		Facts: map[string]any{"distribution": Distribution, "jobs": y.Jobs, "active_jobs": y.ActiveJobs, "threads": y.Threads,
			"system_asp_used_pct": round1(y.SystemASPUsed), "system_asp_gb": gb(y.SystemASPMB), "temporary_storage_gb": gb(y.TempMB),
			"main_storage_gib": math.Round(float64(y.MainStorageKB)/(1<<20)*10) / 10, "cpus": y.CPUs, "processing_units": y.Capacity}}
	if s.Release != "" {
		v := s.Release
		if s.TR > 0 {
			v += " TR" + strconv.Itoa(s.TR)
		}
		r.Facts["version"] = v
	}
	if y.MachineType != "" {
		r.Facts["model"] = strings.TrimSuffix(y.MachineType+"-"+y.MachineModel, "-")
	}
	if y.Serial != "" {
		r.Facts["serial"] = y.Serial
	}
	if y.HostName != "" {
		r.Facts["host_name"] = strings.ToLower(y.HostName)
	}
	if y.PartitionID > 0 {
		r.Facts["partition_id"] = y.PartitionID
	}
	if s.CPUPct != nil {
		r.Facts["cpu_pct"] = *s.CPUPct
	}
	r.Facts["unanswered_inquiries"], r.Facts["severe_messages"] = s.Unanswered, s.SevereCount
	var why []string
	if y.Attention {
		why = append(why, "the attention light is on: a reference code waits on its control panel")
	}
	if y.Restricted {
		why = append(why, "in restricted state: only the console works")
	}
	keys := make([]string, 0, len(s.Problems))
	for k := range s.Problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		why = append(why, "can't read its "+k+": "+short(s.Problems[k], 80))
	}
	if len(why) > 0 {
		r.State, r.Word = report.StateDegraded, upper(strings.Join(why, "; "))
	}
	return r
}

// aspRecord is one ASP. A varied-off independent ASP is said, not judged:
// PowerHA keeps it so on the node that isn't using it.
func aspRecord(a ASP, bad []Disk) report.Workload {
	id := "asp/" + strconv.Itoa(a.Number)
	name := id
	if a.Number > 32 && a.Name != "" {
		name = "asp/" + a.Name // an independent ASP, by its device
	}
	r := report.Workload{Kind: report.KindCluster, ID: id, Name: name, State: report.StateOK, Word: "Varied on",
		Facts: map[string]any{"number": a.Number, "disk_units": a.Units}}
	if a.Type != "" {
		r.Facts["type"] = strings.ToLower(a.Type)
	}
	if a.Threshold > 0 {
		r.Facts["threshold_pct"] = a.Threshold
	}
	var used float64
	if a.TotalMB > 0 {
		used = round1(100 * float64(a.TotalMB-a.FreeMB) / float64(a.TotalMB))
		r.Facts["used_pct"], r.Facts["capacity_gb"] = used, gb(a.TotalMB)
		r.Word = fmt.Sprintf("%.0f%% used of %s", used, size(a.TotalMB))
	}
	if a.OverflowMB > 0 {
		r.Facts["overflow_gb"] = gb(a.OverflowMB)
	}
	var why []string
	switch a.State {
	case "FAILED":
		r.State = report.StateDown
		why = append(why, "Failed")
	case "VARIED OFF":
		r.Word = "Varied off"
		return r
	}
	switch a.Present {
	case "NONE":
		r.State = report.StateDown
		why = append(why, "none of its disk units were found")
	case "SOME":
		r.State = worse(r.State, report.StateDegraded)
		why = append(why, "some of its disk units weren't found")
	}
	if a.Threshold > 0 && a.TotalMB > 0 && used >= float64(a.Threshold) {
		r.State = worse(r.State, report.StateDegraded)
		why = append(why, fmt.Sprintf("%.0f%% used, past IBM i's threshold of %d%%", used, a.Threshold))
	}
	if a.OverflowMB > 0 {
		r.State = worse(r.State, report.StateDegraded)
		why = append(why, "overflowed "+size(a.OverflowMB)+" into the system ASP")
	}
	// A disk unit in trouble is the ASP's: device parity or mirroring
	// keeps the data while one unit is out, so it's degraded, not down.
	for k, d := range bad {
		if k == 3 {
			why = append(why, fmt.Sprintf("%d more disk units", len(bad)-3))
			break
		}
		unit := fmt.Sprintf("disk unit %d", d.Unit)
		if d.Resource != "" {
			unit += " (" + d.Resource + ")"
		}
		why = append(why, unit+" "+diskTrouble(d))
	}
	if len(bad) > 0 {
		r.State = worse(r.State, report.StateDegraded)
		r.Facts["disk_units_in_trouble"] = len(bad)
	}
	if len(why) > 0 {
		r.Word = upper(strings.Join(why, "; "))
	}
	return r
}

// Usage is the partition's CPU over the last minute, in processing
// units, for its history; nil until there's a minute to tell.
func Usage(s *Snapshot) []report.Usage {
	if s.CPUPct == nil || s.System.Capacity <= 0 {
		return nil
	}
	return []report.Usage{{ID: report.ClusterSelf, CPU: math.Round(*s.CPUPct*s.System.Capacity) / 100, CPUOf: s.System.Capacity}}
}

// Unreachable is the last records with the partition's own record saying
// it doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": Distribution}}
	for _, l := range last {
		if l.ID == report.ClusterSelf {
			r = l
			continue
		}
		out = append(out, l)
	}
	r.State, r.Word = report.StateDown, "Mapepire doesn't answer: "+short(err.Error(), 160)
	return append([]report.Workload{r}, out...)
}

func worse(a, b string) string {
	rank := map[string]int{report.StateOK: 0, report.StateUpdating: 1, report.StateUnknown: 2, report.StateDegraded: 3, report.StateDown: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// gb is millions of bytes in GB, as IBM i counts storage.
func gb(mb int64) float64 { return math.Round(float64(mb)/100) / 10 }

func size(mb int64) string {
	if mb >= 1_000_000 {
		return fmt.Sprintf("%.1f TB", float64(mb)/1e6)
	}
	return fmt.Sprintf("%.0f GB", float64(mb)/1e3)
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
