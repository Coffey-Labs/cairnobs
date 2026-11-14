package nutanix

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Records makes a snapshot into the records a cluster agent sends:
// Prism Central itself, its clusters' AHV hosts as nodes, its VMs, and
// its storage containers as volumes. Prism's open alerts are noted on the
// records they're about. Usage isn't read: v4 keeps it in a separate
// stats API, for later.
func Records(s *Snapshot) []report.Workload {
	clusterName := map[string]string{}
	for _, c := range s.Clusters {
		clusterName[c.ExtID] = c.Name
	}
	alerts := alertsByEntity(s.Alerts)
	recs := []report.Workload{centralRecord(s, alerts)}
	hostName := map[string]string{}
	for _, h := range s.Hosts {
		name := h.HostName
		if name == "" {
			name = h.ExtID
		}
		hostName[h.ExtID] = name
		r := report.Workload{Kind: report.KindNode, ID: h.ExtID, Name: name, State: report.StateOK, Word: "Connected",
			Facts: map[string]any{"cluster": h.Cluster.Name, "cpus": int(h.NumberOfCores), "memory_gib": gib(h.MemorySizeBytes)}}
		if h.Hypervisor.FullName != "" {
			r.Facts["version"] = h.Hypervisor.FullName
		}
		if h.BlockModel != "" {
			r.Facts["model"] = h.BlockModel
		}
		if h.HostType != "" && h.HostType != "HYPER_CONVERGED" {
			r.Facts["type"] = strings.ToLower(strings.ReplaceAll(h.HostType, "_", "-"))
		}
		switch hv := h.Hypervisor; {
		case hv.AcropolisConnectionState == "DISCONNECTED":
			r.State, r.Word = report.StateDown, "Disconnected from its cluster"
		case hv.State == "ENTERED_MAINTENANCE_MODE":
			r.State, r.Word = report.StateUpdating, "In maintenance mode"
		case hv.State == "ENTERING_MAINTENANCE_MODE", hv.State == "ENTERING_MAINTENANCE_MODE_FROM_HA_FAILOVER":
			r.State, r.Word = report.StateUpdating, "Entering maintenance mode"
		case hv.State == "HA_FAILOVER_SOURCE", hv.State == "HA_HEALING_SOURCE":
			r.State, r.Word = report.StateDown, "HA is moving its VMs off it"
		case h.NodeStatus == "TO_BE_REMOVED" || h.NodeStatus == "OK_TO_BE_REMOVED":
			r.State, r.Word = report.StateUpdating, "Being removed from its cluster"
		case h.IsDegraded:
			r.State, r.Word = report.StateDegraded, "Marked degraded by its cluster"
		}
		noteAlerts(&r, alerts[h.ExtID])
		recs = append(recs, r)
	}
	// Names are unique per cluster, if that: a name two VMs share gets its
	// id after it, so each keeps its own history and alerts.
	count := map[string]int{}
	for _, v := range s.VMs {
		count[v.Name]++
	}
	for _, v := range s.VMs {
		name := v.Name
		if name == "" || count[name] > 1 {
			name = strings.TrimSpace(name + " (" + v.ExtID + ")")
		}
		threads := max(v.NumThreadsPerCore, 1)
		r := report.Workload{Kind: report.KindVM, ID: v.ExtID, Name: name, State: report.StateOK, Word: "Running",
			Facts: map[string]any{"hypervisor": "AHV", "vcpus": v.NumSockets * max(v.NumCoresPerSocket, 1) * threads, "memory_gib": gib(v.MemorySizeBytes)}}
		if v.Host != nil {
			r.Node = hostName[v.Host.ExtID]
		}
		if v.Cluster != nil && clusterName[v.Cluster.ExtID] != "" {
			r.Facts["cluster"] = clusterName[v.Cluster.ExtID]
		}
		if v.BiosUUID != "" {
			r.Facts["firmware_uuid"] = strings.ToLower(v.BiosUUID)
		}
		if v.IsAgentVM {
			r.Facts["agent_vm"] = true
		}
		switch v.PowerState {
		case "ON":
		case "OFF":
			r.State, r.Word = report.StateStopped, "Powered off"
		case "PAUSED":
			r.State, r.Word = report.StateStopped, "Paused"
		default:
			r.State, r.Word = report.StateUnknown, "Its power state can't be told"
		}
		if g := v.GuestTools; g != nil {
			if g.GuestOSVersion != "" {
				r.Facts["guest_os"] = g.GuestOSVersion
			}
			if g.IsInstalled && !g.IsReachable && r.State == report.StateOK {
				r.Facts["guest_tools"] = "not reachable"
			}
		}
		noteAlerts(&r, alerts[v.ExtID])
		recs = append(recs, r)
	}
	for _, c := range s.Containers {
		if c.IsInternal {
			continue // Nutanix's own (NutanixManagementShare and the like)
		}
		cl := c.ClusterName
		if cl == "" {
			cl = clusterName[c.ClusterExtID]
		}
		r := report.Workload{Kind: report.KindPVC, ID: c.ExtID, Name: c.Name, State: report.StateOK, Word: "Available",
			Facts: map[string]any{"type": "storage container", "cluster": cl, "capacity_gib": gib(c.MaxCapacityBytes)}}
		if c.ReplicationFactor > 0 {
			r.Facts["replication_factor"] = c.ReplicationFactor
		}
		if c.IsMarkedForRemoval {
			r.State, r.Word = report.StateUpdating, "Marked for removal"
		}
		noteAlerts(&r, alerts[c.ExtID])
		recs = append(recs, r)
	}
	sort.SliceStable(recs[1:], func(i, j int) bool { return recs[1+i].ID < recs[1+j].ID })
	return recs
}

func centralRecord(s *Snapshot, alerts map[string][]Alert) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: s.Name, State: report.StateOK, Word: "API answering",
		Facts: map[string]any{"distribution": "Prism Central", "hosts": len(s.Hosts)}}
	var clusters, unavailable []string
	var mine []Alert // about Prism Central or one of its clusters, named
	for _, c := range s.Clusters {
		for _, a := range alerts[c.ExtID] {
			a.Title = c.Name + ": " + a.Title
			mine = append(mine, a)
		}
		if c.isPrismCentral() {
			if v := c.Config.BuildInfo.Version; v != "" {
				r.Facts["version"] = v
			}
			continue
		}
		clusters = append(clusters, c.Name)
		if c.Config.IsAvailable != nil && !*c.Config.IsAvailable {
			unavailable = append(unavailable, c.Name)
		}
	}
	sort.Strings(clusters)
	r.Facts["clusters"] = strings.Join(clusters, ", ")
	var parts []string
	if len(unavailable) > 0 {
		sort.Strings(unavailable)
		parts = append(parts, "cluster "+strings.Join(unavailable, ", ")+" not available")
	}
	keys := make([]string, 0, len(s.Problems))
	for k := range s.Problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "can't read "+k+": "+short(s.Problems[k], 80))
	}
	if len(parts) > 0 {
		r.State, r.Word = report.StateDegraded, upper(strings.Join(parts, "; "))
	}
	noteAlerts(&r, mine)
	return r
}

// alertsByEntity files each open alert under the entities it's about:
// its source and those it affects.
func alertsByEntity(as []Alert) map[string][]Alert {
	out := map[string][]Alert{}
	for _, a := range as {
		seen := map[string]bool{}
		add := func(id string) {
			if id != "" && !seen[id] {
				seen[id] = true
				out[id] = append(out[id], a)
			}
		}
		if a.SourceEntity != nil {
			add(a.SourceEntity.ExtID)
		}
		for _, e := range a.AffectedEntities {
			add(e.ExtID)
		}
	}
	return out
}

// noteAlerts lists Prism's open alerts on a record. A critical one nobody
// has acknowledged makes a record that's otherwise fine degraded: Prism
// judged it, and says why.
func noteAlerts(r *report.Workload, as []Alert) {
	if len(as) == 0 {
		return
	}
	var all, crit []string
	for _, a := range as {
		t := a.Title
		if t == "" {
			t = a.ExtID
		}
		all = append(all, fmt.Sprintf("%s (%s)", t, strings.ToLower(a.Severity)))
		if a.Severity == "CRITICAL" && !a.IsAcknowledged {
			crit = append(crit, t)
		}
	}
	sort.Strings(all)
	r.Facts["alerts"] = strings.Join(all, ", ")
	if len(crit) > 0 && r.State == report.StateOK {
		sort.Strings(crit)
		r.State, r.Word = report.StateDegraded, "Alert from Prism: "+strings.Join(crit, ", ")
	}
}

func gib(b int64) float64 { return math.Round(float64(b)/(1<<30)*10) / 10 }

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

// Unreachable is the last records with Prism Central's own record saying
// its API doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": "Prism Central"}}
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
