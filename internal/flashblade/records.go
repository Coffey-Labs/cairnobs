package flashblade

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
// array itself, its blades as nodes, its file systems and buckets (as
// volumes are), and its replication connections as parts of the array.
// Purity//FB judges (docs/pure.md, decision 3): its open alerts go on the
// record they're about, and a critical one makes it degraded; blades,
// hardware, replica links and connections are read as they are. No
// space thresholds of our own.
func Records(s *Snapshot) []report.Workload {
	alerts := map[string][]purerest.Alert{} // by lowercased component name
	for _, a := range s.Alerts {
		if a.Severity == "hidden" {
			continue
		}
		n := strings.ToLower(a.ComponentName)
		alerts[n] = append(alerts[n], a)
	}
	used := map[string]bool{}
	take := func(names ...string) []purerest.Alert {
		var out []purerest.Alert
		for _, n := range names {
			n = strings.ToLower(n)
			if as, ok := alerts[n]; ok && !used[n] {
				used[n] = true
				out = append(out, as...)
			}
		}
		return out
	}
	// Replica links go on the file system or bucket they replicate.
	links := map[string][]Link{} // by "fs/<name>" or "bucket/<name>"
	for _, l := range s.Links {
		switch {
		case l.LocalFS != nil:
			links["fs/"+l.LocalFS.Name] = append(links["fs/"+l.LocalFS.Name], l)
		case l.LocalBucket != nil:
			links["bucket/"+l.LocalBucket.Name] = append(links["bucket/"+l.LocalBucket.Name], l)
		}
	}

	var recs []report.Workload
	for _, b := range s.Blades {
		if b.Status == "unused" {
			continue // an empty slot
		}
		id := strings.ToLower(b.Name)
		r := report.Workload{Kind: report.KindNode, ID: id, Name: b.Name, State: report.StateOK, Word: "Healthy", Facts: map[string]any{}}
		if b.RawCapacity > 0 {
			r.Facts["raw_tib"] = tib(b.RawCapacity)
		}
		why := ""
		if b.Details != "" {
			why = ": " + short(b.Details, 80)
		}
		switch b.Status {
		case "healthy":
		case "identifying":
			r.Word = "Healthy, its light on to be found"
		case "evacuating":
			r.State, r.Word = report.StateUpdating, fmt.Sprintf("Evacuating its data (%.0f%%)", b.Progress*100)
		case "evacuated":
			r.State, r.Word = report.StateStopped, "Evacuated: ready to be taken out"
		case "critical":
			r.State, r.Word = report.StateDown, "Critical"+why
		case "unhealthy":
			r.State, r.Word = report.StateDegraded, "Unhealthy"+why
		default:
			r.State, r.Word = report.StateUnknown, upper(orUnknown(b.Status))+why
		}
		var own []string
		for n := range alerts {
			if n == id || strings.HasPrefix(n, id+".") {
				own = append(own, n)
			}
		}
		sort.Strings(own)
		purerest.NoteAlerts(&r, take(own...))
		recs = append(recs, r)
	}
	for _, f := range s.FileSystems {
		r := report.Workload{Kind: report.KindPVC, ID: f.ID, Name: f.Name, State: report.StateOK,
			Facts: map[string]any{"type": "file system", "provisioned_gib": gib(f.Provisioned), "used_gib": gib(f.Space.Used())}}
		if f.Space.DataReduction > 0 {
			r.Facts["data_reduction"] = math.Round(f.Space.DataReduction*10) / 10
		}
		if f.HardLimitEnabled {
			r.Facts["hard_limit"] = true
		}
		if f.Created > 0 {
			r.Since = time.UnixMilli(f.Created).UTC()
		}
		var protos []string
		if f.NFS != nil && f.NFS.V3 {
			protos = append(protos, "NFSv3")
		}
		if f.NFS != nil && f.NFS.V41 {
			protos = append(protos, "NFSv4.1")
		}
		if f.SMB != nil && f.SMB.Enabled {
			protos = append(protos, "SMB")
		}
		if f.HTTP != nil && f.HTTP.Enabled {
			protos = append(protos, "HTTP")
		}
		r.Word = "Shared over " + strings.Join(protos, ", ")
		switch {
		case f.PromotionStatus == "demoted":
			r.Word = "Demoted: a replication target, read only"
		case f.Writable != nil && !*f.Writable:
			r.Word = "Read only: writing is turned off"
		case len(protos) == 0:
			r.Word = "Not shared over any protocol"
		}
		if len(protos) > 0 {
			r.Facts["protocols"] = strings.Join(protos, ", ")
		}
		noteLinks(&r, links["fs/"+f.Name])
		purerest.NoteAlerts(&r, take(f.Name))
		recs = append(recs, r)
	}
	for _, b := range s.Buckets {
		r := report.Workload{Kind: report.KindPVC, ID: b.ID, Name: b.Name, State: report.StateOK, Word: "Ready",
			Facts: map[string]any{"type": "bucket", "used_gib": gib(b.Space.Used()), "objects": b.ObjectCount}}
		if b.Account != nil && b.Account.Name != "" {
			r.Facts["account"] = b.Account.Name
		}
		if b.QuotaLimit != nil && *b.QuotaLimit > 0 {
			r.Facts["quota_gib"] = gib(*b.QuotaLimit)
		}
		if b.Versioning != "" && b.Versioning != "none" {
			r.Facts["versioning"] = b.Versioning
		}
		if b.Created > 0 {
			r.Since = time.UnixMilli(b.Created).UTC()
		}
		noteLinks(&r, links["bucket/"+b.Name])
		purerest.NoteAlerts(&r, take(b.Name))
		recs = append(recs, r)
	}
	for _, c := range s.Connections {
		r := report.Workload{Kind: report.KindCluster, ID: "connection/" + c.ID, Name: "connection/" + c.Remote.Name, State: report.StateOK, Word: "Connected",
			Facts: map[string]any{"type": c.Type}}
		if c.Version != "" {
			r.Facts["version"] = c.Version
		}
		switch c.Status {
		case "connected":
		case "partially_connected":
			r.State, r.Word = report.StateDegraded, "Partly connected: some replication addresses don't answer"
		case "connecting":
			r.State, r.Word = report.StateDown, "Not connected (trying)"
		case "incompatible":
			r.State, r.Word = report.StateDown, "Incompatible: the two arrays' versions can't replicate to each other"
		default:
			r.State, r.Word = report.StateUnknown, upper(orUnknown(c.Status))
		}
		purerest.NoteAlerts(&r, take(c.Remote.Name))
		recs = append(recs, r)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	var rest []purerest.Alert
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

// noteLinks puts a file system's or bucket's replication on it: where to
// or from, how far behind; an unhealthy link makes it degraded.
func noteLinks(r *report.Workload, ls []Link) {
	if len(ls) == 0 {
		return
	}
	var all, bad []string
	for _, l := range ls {
		peer := "another array"
		if l.Remote != nil && l.Remote.Name != "" {
			peer = l.Remote.Name
		}
		w := "to " + peer
		if l.Direction == "inbound" {
			w = "from " + peer
		}
		st := orUnknown(l.Status)
		if l.Paused && l.Status != "paused" {
			st = "paused"
		}
		w += ", " + st
		if l.Lag != nil && *l.Lag > 0 {
			w += ", " + behind(time.Duration(*l.Lag)*time.Millisecond)
		}
		all = append(all, w)
		if l.Status == "unhealthy" {
			d := "replication " + strings.SplitN(w, ",", 2)[0] + " is unhealthy"
			if l.StatusDetails != "" {
				d += ": " + short(l.StatusDetails, 80)
			}
			bad = append(bad, d)
		}
	}
	sort.Strings(all)
	r.Facts["replication"] = strings.Join(all, "; ")
	if len(bad) > 0 && r.State == report.StateOK {
		sort.Strings(bad)
		r.State, r.Word = report.StateDegraded, upper(strings.Join(bad, "; "))
	}
}

func behind(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute behind"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min behind", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.0f h behind", d.Hours())
	}
}

func arrayRecord(s *Snapshot, alerts []purerest.Alert) report.Workload {
	a := s.Array
	name := a.Name
	if name == "" {
		name = s.Name
	}
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "API answering",
		Facts: map[string]any{"distribution": "Purity//FB"}}
	if a.OS != "" {
		r.Facts["distribution"] = a.OS
	}
	if a.Version != "" {
		r.Facts["version"] = a.Version
	}
	if a.ProductType != "" {
		r.Facts["model"] = a.ProductType
	}
	var why []string
	if sp := s.Space; sp != nil {
		r.Facts["capacity_tib"] = tib(sp.Capacity)
		if sp.Capacity > 0 {
			r.Facts["used_pct"] = math.Round(100 * float64(sp.Space.Used()) / float64(sp.Capacity))
		}
		if sp.Space.DataReduction > 0 {
			r.Facts["data_reduction"] = math.Round(sp.Space.DataReduction*10) / 10
		}
		if sp.Parity != nil && *sp.Parity < 1 {
			why = append(why, fmt.Sprintf("rebuilding its data redundancy (parity %.2f)", *sp.Parity))
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
		why = append([]string{"hardware: " + strings.Join(parts, ", ")}, why...)
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
	purerest.NoteAlerts(&r, alerts)
	return r
}

// Usage is the array's performance and space now, for its history
// (docs/pure.md, step 4); nil when its performance couldn't be read.
func Usage(s *Snapshot) []report.Usage {
	if s.Perf == nil {
		return nil
	}
	p := s.Perf
	io := &report.IO{ReadOps: p.ReadsPerSec, WriteOps: p.WritesPerSec, ReadUs: p.UsecPerReadOp, WriteUs: p.UsecPerWriteOp,
		ReadBps: p.ReadBytesPerSec, WriteBps: p.WriteBytesPerSec}
	if s.Space != nil {
		io.Used, io.UsedOf = s.Space.Space.Used(), s.Space.Capacity
	}
	return []report.Usage{{ID: report.ClusterSelf, IO: io}}
}

// Unreachable is the last records with the array's own record saying its
// API doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": "Purity//FB"}}
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
