package flasharray

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/purerest"
	"github.com/cairnobs/cairnobs/internal/report"
)

// Records makes a snapshot into the records a cluster agent sends: the
// array itself, its controllers as nodes, its volumes, and its pods and
// replication connections as parts of the array, as OpenShift's operators
// are. Purity judges the array (docs/pure.md, decision 3): its open
// alerts go on the record they're about, and a critical one makes it
// degraded; controllers, hardware, pods and connections are read as they
// are. No space thresholds of our own.
func Records(s *Snapshot) []report.Workload {
	alerts := map[string][]Alert{} // by lowercased component name
	for _, a := range s.Alerts {
		if a.Severity == "hidden" {
			continue
		}
		alerts[strings.ToLower(a.ComponentName)] = append(alerts[strings.ToLower(a.ComponentName)], a)
	}
	used := map[string]bool{} // the components an alert was put on
	take := func(names ...string) []Alert {
		var out []Alert
		for _, n := range names {
			n = strings.ToLower(n)
			if as, ok := alerts[n]; ok && !used[n] {
				used[n] = true
				out = append(out, as...)
			}
		}
		return out
	}
	var recs []report.Workload
	for _, c := range s.Controllers {
		if c.Type == "shelf_controller" {
			continue // a shelf's: trouble with it shows as hardware
		}
		r := report.Workload{Kind: report.KindNode, ID: strings.ToLower(c.Name), Name: c.Name, State: report.StateOK, Word: "Ready",
			Facts: map[string]any{"mode": c.Mode}}
		if c.Model != "" {
			r.Facts["model"] = c.Model
		}
		if c.Version != "" {
			r.Facts["version"] = c.Version
		}
		switch {
		case c.Mode == "offline" || c.Mode == "not present":
			r.State, r.Word = report.StateDown, upper(c.Mode)
		case c.Status == "not ready":
			r.State, r.Word = report.StateDown, "Not ready"
		case c.Status == "updating":
			r.State, r.Word = report.StateUpdating, "Updating"
		case c.Status == "unknown":
			r.State, r.Word = report.StateUnknown, "Its state is unknown to the array"
		case c.Mode == "primary":
			r.Word = "Ready, primary"
		case c.Mode == "secondary":
			r.Word = "Ready, secondary"
		}
		// Alerts name a controller's parts ("ct0.eth2"): they go on it.
		var own []string
		for n := range alerts {
			if n == r.ID || strings.HasPrefix(n, r.ID+".") {
				own = append(own, n)
			}
		}
		sort.Strings(own)
		noteAlerts(&r, take(own...))
		recs = append(recs, r)
	}
	for _, v := range s.Volumes {
		if v.Subtype == "protocol_endpoint" {
			continue // a vVols plumbing volume, not data
		}
		r := report.Workload{Kind: report.KindPVC, ID: v.ID, Name: v.Name, State: report.StateOK, Word: "Connected",
			Facts: map[string]any{"type": "volume", "provisioned_gib": gib(v.Provisioned), "used_gib": gib(v.Space.Unique + v.Space.Snapshots),
				"hosts": v.ConnectionCount}}
		if v.Serial != "" {
			r.Facts["serial"] = strings.ToLower(v.Serial)
		}
		if v.Space.DataReduction > 0 {
			r.Facts["data_reduction"] = math.Round(v.Space.DataReduction*10) / 10
		}
		if v.Pod != nil && v.Pod.Name != "" {
			r.Facts["pod"] = v.Pod.Name
		}
		if v.VolumeGroup != nil && v.VolumeGroup.Name != "" {
			r.Facts["volume_group"] = v.VolumeGroup.Name
		}
		if v.Created > 0 {
			r.Since = time.UnixMilli(v.Created).UTC()
		}
		switch {
		case v.PromotionStatus == "demoted":
			r.Word = "Demoted: a replication target, read only"
		case v.ConnectionCount == 0:
			r.Word = "Not connected to a host"
		}
		noteAlerts(&r, take(v.Name))
		recs = append(recs, r)
	}
	for _, p := range s.Pods {
		r := report.Workload{Kind: report.KindCluster, ID: "pod/" + p.ID, Name: "pod/" + p.Name, State: report.StateOK, Word: "In sync",
			Facts: map[string]any{"arrays": len(p.Arrays)}}
		var bad []string
		for _, a := range p.Arrays {
			switch a.Status {
			case "online":
			case "resyncing":
				r.State = worse(r.State, report.StateUpdating)
				bad = append(bad, fmt.Sprintf("resyncing on %s (%.0f%%)", a.Name, a.Progress*100))
			case "suspended":
				r.State = worse(r.State, report.StateUpdating)
				bad = append(bad, "suspended for a moment on "+a.Name)
			case "offline":
				r.State = worse(r.State, report.StateDegraded)
				bad = append(bad, "offline on "+a.Name)
			default:
				r.State = worse(r.State, report.StateUnknown)
				bad = append(bad, orUnknown(a.Status)+" on "+a.Name)
			}
			if a.MediatorStatus != "" && a.MediatorStatus != "online" && a.Status == "online" {
				r.State = worse(r.State, report.StateDegraded)
				bad = append(bad, "mediator "+a.MediatorStatus+" from "+a.Name)
			}
		}
		if len(p.Arrays) == 1 {
			r.Word = "On this array only"
		}
		if len(bad) > 0 {
			r.Word = upper(strings.Join(bad, "; "))
		}
		if p.PromotionStatus == "demoted" {
			r.Facts["promotion"] = "demoted"
		}
		noteAlerts(&r, take(p.Name))
		recs = append(recs, r)
	}
	for _, c := range s.Connections {
		r := report.Workload{Kind: report.KindCluster, ID: "connection/" + c.ID, Name: "connection/" + c.Name, State: report.StateOK, Word: "Connected",
			Facts: map[string]any{"type": c.Type}}
		if c.Version != "" {
			r.Facts["version"] = c.Version
		}
		switch c.Status {
		case "connected":
		case "partially_connected":
			r.State, r.Word = report.StateDegraded, "Partly connected: some replication addresses don't answer"
		case "unbalanced":
			r.State, r.Word = report.StateDegraded, "Connected, but its paths aren't redundant"
		case "connecting":
			r.State, r.Word = report.StateDown, "Not connected (trying)"
		default:
			r.State, r.Word = report.StateUnknown, upper(orUnknown(c.Status))
		}
		noteAlerts(&r, take(c.Name))
		recs = append(recs, r)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	// What no record took goes on the array's own.
	var rest []Alert
	keys := make([]string, 0, len(alerts))
	for n := range alerts {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		if !used[n] {
			rest = append(rest, alerts[n]...)
		}
	}
	return append([]report.Workload{arrayRecord(s, rest)}, recs...)
}

func arrayRecord(s *Snapshot, alerts []Alert) report.Workload {
	a := s.Array
	name := a.Name
	if name == "" {
		name = s.Name
	}
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "API answering",
		Facts: map[string]any{"distribution": "Purity//FA", "capacity_tib": tib(a.Capacity)}}
	if a.OS != "" {
		r.Facts["distribution"] = a.OS
	}
	if a.Version != "" {
		r.Facts["version"] = a.Version
	}
	if a.Capacity > 0 {
		r.Facts["used_pct"] = math.Round(100 * float64(a.Space.TotalPhysical) / float64(a.Capacity))
	}
	if a.Space.DataReduction > 0 {
		r.Facts["data_reduction"] = math.Round(a.Space.DataReduction*10) / 10
	}
	for _, c := range s.Controllers {
		if c.Type != "shelf_controller" && c.Model != "" {
			r.Facts["model"] = c.Model
			break
		}
	}
	var parts []string
	for _, p := range s.Unhealthy {
		w := p.Name + " " + strings.ReplaceAll(p.Status, "_", " ")
		if p.Details != "" {
			w += " (" + short(p.Details, 60) + ")"
		}
		parts = append(parts, w)
	}
	sort.Strings(parts)
	if len(parts) > 0 {
		r.Facts["hardware"] = strings.Join(parts, ", ")
	}
	var why []string
	if len(parts) > 0 {
		why = append(why, "hardware: "+strings.Join(parts, ", "))
	}
	if a.Parity != nil && *a.Parity < 1 {
		why = append(why, fmt.Sprintf("rebuilding its data redundancy (parity %.2f)", *a.Parity))
	}
	keys := make([]string, 0, len(s.Problems))
	for k := range s.Problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		why = append(why, "can't read "+k+": "+short(s.Problems[k], 80))
	}
	if len(why) > 0 {
		r.State, r.Word = report.StateDegraded, upper(strings.Join(why, "; "))
	}
	if s.PerfErr != "" {
		r.Facts["performance"] = "can't read it: " + short(s.PerfErr, 80)
	}
	noteAlerts(&r, alerts)
	return r
}

// Usage is the array's performance and space now, for its history
// (docs/pure.md, step 4); nil when its performance couldn't be read.
func Usage(s *Snapshot) []report.Usage {
	if s.Perf == nil {
		return nil
	}
	p := s.Perf
	return []report.Usage{{ID: report.ClusterSelf, IO: &report.IO{
		ReadOps: p.ReadsPerSec, WriteOps: p.WritesPerSec, ReadUs: p.UsecPerReadOp, WriteUs: p.UsecPerWriteOp,
		ReadBps: p.ReadBytesPerSec, WriteBps: p.WriteBytesPerSec, Used: s.Array.Space.TotalPhysical, UsedOf: s.Array.Capacity}}}
}

// noteAlerts lists Purity's open alerts on a record.
func noteAlerts(r *report.Workload, as []Alert) { purerest.NoteAlerts(r, as) }

// worse is the worse of two states, as a pod's arrays add up.
func worse(a, b string) string {
	rank := map[string]int{report.StateOK: 0, report.StateUpdating: 1, report.StateUnknown: 2, report.StateDegraded: 3, report.StateDown: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.ReplaceAll(s, "_", " ")
}

func gib(b int64) float64 { return math.Round(float64(b)/(1<<30)*10) / 10 }
func tib(b int64) float64 { return math.Round(float64(b)/(1<<40)*10) / 10 }

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

// Unreachable is the last records with the array's own record saying its
// API doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": "Purity//FA"}}
	for _, r := range last {
		if r.ID == report.ClusterSelf {
			self = r
			continue
		}
		out = append(out, r)
	}
	self.State, self.Word = report.StateDown, "Can't reach its API: "+short(err.Error(), 160)
	return append([]report.Workload{self}, out...)
}
