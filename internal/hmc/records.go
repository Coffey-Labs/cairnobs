package hmc

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Distribution is what an HMC's own record says it is.
const Distribution = "HMC"

// Hypervisor is what a partition's record says runs it.
const Hypervisor = "PowerVM"

// Records makes a snapshot into the records a cluster agent sends: the HMC
// itself, its Power servers as nodes, and their partitions and Virtual
// I/O Servers as VMs. Usage isn't read: the HMC keeps it in its
// performance and capacity monitoring, which has to be turned on, for
// later.
func Records(s *Snapshot) []report.Workload {
	recs := []report.Workload{consoleRecord(s)}
	// Partition names are unique on a server, not across them: a name two
	// partitions share gets its server after it.
	count := map[string]int{}
	for _, sv := range s.Servers {
		for _, p := range sv.Partitions {
			count[p.Name]++
		}
	}
	for _, sv := range s.Servers {
		recs = append(recs, serverRecord(sv))
		up := sv.State != "" && strings.EqualFold(sv.State, "operating")
		for _, p := range sv.Partitions {
			r := partitionRecord(sv, p, up)
			if count[p.Name] > 1 {
				r.Name += " (" + sv.Name + ")"
			}
			recs = append(recs, r)
		}
	}
	sort.SliceStable(recs[1:], func(i, j int) bool {
		a, b := recs[1+i], recs[1+j]
		if a.Kind != b.Kind {
			return a.Kind == report.KindNode
		}
		return a.Name < b.Name
	})
	return recs
}

func consoleRecord(s *Snapshot) report.Workload {
	name := s.Name
	if s.Console != "" {
		name = s.Console
	}
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "API answering",
		Facts: map[string]any{"distribution": Distribution, "servers": len(s.Servers)}}
	if len(s.Problems) > 0 {
		keys := make([]string, 0, len(s.Problems))
		for k := range s.Problems {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+": "+short(s.Problems[k], 80))
		}
		r.State, r.Word = report.StateDegraded, "Can't read "+strings.Join(parts, "; ")
	}
	return r
}

// serverRecord is a Power server: judged by the HMC's own state for it,
// and its attention light, which firmware turns on with a serviceable
// event open.
func serverRecord(sv Server) report.Workload {
	id := sv.UUID
	if id == "" {
		id = sv.Name
	}
	r := report.Workload{Kind: report.KindNode, ID: id, Name: sv.Name, State: report.StateOK, Word: "Operating",
		Facts: map[string]any{"model": model(sv.MachineType, sv.Model), "serial": strings.TrimSpace(sv.Serial),
			"processor_units": sv.ProcUnits, "processor_units_free": sv.ProcFree,
			"memory_gib": round1(float64(sv.MemMB) / 1024), "memory_free_gib": round1(float64(sv.MemFreeMB) / 1024)}}
	running := 0
	for _, p := range sv.Partitions {
		if strings.EqualFold(p.State, "running") {
			running++
		}
	}
	r.Facts["partitions"], r.Facts["partitions_running"] = len(sv.Partitions), running
	state := strings.ToLower(strings.TrimSpace(sv.State))
	switch {
	case state == "operating":
		r.Word = fmt.Sprintf("Operating, %d of %d partitions running", running, len(sv.Partitions))
		if sv.AttentionLED == "true" {
			r.State, r.Word = report.StateDegraded, "Its attention light is on: a serviceable event is open on the HMC"
		}
	case state == "standby":
		r.State, r.Word = report.StateStopped, "In standby: powered on, no partitions started"
	case state == "power off":
		r.State, r.Word = report.StateStopped, "Powered off"
	case state == "powering off":
		r.State, r.Word = report.StateUpdating, "Powering off"
	case state == "power on", state == "initializing":
		r.State, r.Word = report.StateUpdating, "Powering on"
	case strings.HasPrefix(state, "error"):
		r.State, r.Word = report.StateDown, "Error ("+sv.State+")"
	case state == "no connection":
		r.State, r.Word = report.StateDown, "The HMC has no connection to its service processor"
	case state == "failed authentication", state == "pending authentication":
		r.State, r.Word = report.StateDegraded, "The HMC can't sign in to its service processor ("+state+")"
	case state == "incomplete", state == "recovery":
		r.State, r.Word = report.StateDegraded, "The HMC can't read all of it ("+state+")"
	case state == "version mismatch":
		r.State, r.Word = report.StateDegraded, "Its firmware is newer than the HMC supports (version mismatch)"
	default:
		r.State, r.Word = report.StateUnknown, "The HMC says "+sv.State
	}
	if d := strings.TrimSpace(sv.DetailedState); d != "" && !strings.EqualFold(d, "none") {
		r.Facts["detailed_state"] = d
	}
	return r
}

// partitionRecord is a partition or VIOS: judged by the HMC's state for
// it. up says whether its server is operating: a server that isn't has
// said so already, on its own record.
func partitionRecord(sv Server, p Partition, up bool) report.Workload {
	id := strings.ToLower(strings.TrimSpace(p.UUID))
	if id == "" {
		id = fmt.Sprintf("%s/%d", sv.Name, p.ID)
	}
	r := report.Workload{Kind: report.KindVM, ID: id, Name: p.Name, Node: sv.Name, State: report.StateOK, Word: "Running",
		Facts: map[string]any{"hypervisor": Hypervisor, "os": osOf(p), "partition_id": p.ID, "server_serial": strings.TrimSpace(sv.Serial),
			"server_model": model(sv.MachineType, sv.Model), "memory_gib": round1(float64(p.MemMB) / 1024)}}
	if v := strings.TrimSpace(p.OS); v != "" && v != "Unknown" && strings.Trim(v, "0.") != "" {
		r.Facts["guest_os"] = v
	}
	if p.Dedicated == "true" {
		r.Facts["vcpus"], r.Facts["processors"] = p.DedProcs, "dedicated"
	} else {
		r.Facts["vcpus"], r.Facts["processing_units"] = p.VirtProcs, p.ProcUnits
		if p.SharedMode != "" {
			r.Facts["processors"] = "shared, " + p.SharedMode
		}
	}
	ref := strings.TrimSpace(p.RefCode)
	if ref != "" && strings.Trim(ref, "0") != "" {
		r.Facts["reference_code"] = ref
	}
	if p.Type != "OS400" && p.RMC != "" {
		r.Facts["rmc"] = p.RMC
	}
	withRef := func(w string) string {
		if ref != "" && strings.Trim(ref, "0") != "" {
			return w + ", reference code " + ref
		}
		return w
	}
	switch state := strings.ToLower(strings.TrimSpace(p.State)); state {
	case "running":
		switch {
		case p.Attention == "true":
			r.State, r.Word = report.StateDegraded, "Running, with its attention light on"
		case p.VIOS() && p.RMC != "" && p.RMC != "active":
			// The HMC reaches a VIOS over RMC to change its mappings and
			// move partitions; without it, neither works.
			r.State, r.Word = report.StateDegraded, "Running, but the HMC can't reach it over RMC ("+p.RMC+")"
		}
	case "not activated":
		r.State, r.Word = report.StateStopped, "Not activated"
		if p.VIOS() && up {
			r.State, r.Word = report.StateDegraded, "Not activated: the partitions it serves have one path less"
		}
	case "starting":
		r.State, r.Word = report.StateStarting, withRef("Starting")
	case "shutting down":
		r.State, r.Word = report.StateUpdating, "Shutting down"
	case "migrating running", "migrating not active":
		r.State, r.Word = report.StateUpdating, "Moving to another server (Live Partition Mobility)"
	case "suspending", "resuming", "hardware discovery":
		r.State, r.Word = report.StateUpdating, strings.ToUpper(state[:1])+state[1:]
	case "suspended":
		r.State, r.Word = report.StateStopped, "Suspended"
	case "open firmware":
		r.State, r.Word = report.StateDown, "Stopped in Open Firmware: its operating system isn't running"
	case "error":
		r.State, r.Word = report.StateDown, withRef("Error")
	default:
		r.State, r.Word = report.StateUnknown, "The HMC says "+p.State
	}
	if !up && r.State != report.StateStopped {
		r.State, r.Word = report.StateUnknown, "As last known; its server isn't operating"
	}
	return r
}

// osOf is what runs in a partition, for people.
func osOf(p Partition) string {
	switch {
	case p.Type == "OS400":
		return "IBM i"
	case p.VIOS():
		return "VIOS"
	case strings.HasPrefix(p.OS, "AIX"):
		return "AIX"
	case strings.HasPrefix(p.OS, "Linux"):
		return "Linux"
	}
	return "AIX or Linux"
}

// model is "9105-42A".
func model(mt, m string) string {
	mt, m = strings.TrimSpace(mt), strings.TrimSpace(m)
	if mt == "" {
		return m
	}
	return mt + "-" + m
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Unreachable is the last records with the HMC's own record saying its API
// doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": Distribution}}
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
