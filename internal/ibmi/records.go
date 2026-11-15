package ibmi

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Distribution is what a partition's own record says it is.
const Distribution = "IBM i"

// Records makes a snapshot into the records a cluster agent sends: the
// partition itself, and its ASPs as its parts (docs/ibmi.md, decision 3).
// IBM i judges itself: an ASP warns at its own storage threshold, the one
// that sends QSYSOPR CPF0907 or CPI0953; no thresholds of ours.
func Records(s *Snapshot) []report.Workload {
	recs := []report.Workload{self(s)}
	for _, a := range s.ASPs {
		recs = append(recs, aspRecord(a))
	}
	return recs
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
func aspRecord(a ASP) report.Workload {
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
