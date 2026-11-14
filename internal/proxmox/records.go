package proxmox

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Records makes a snapshot into the records a cluster agent sends: the
// cluster itself (its quorum), its nodes, and its VMs and containers as
// vm records, with what the running ones use. Templates aren't VMs.
func Records(s *Snapshot) ([]report.Workload, []report.Usage) {
	var recs []report.Workload
	var use []report.Usage
	recs = append(recs, clusterRecord(s))
	for _, x := range s.Resources {
		switch {
		case x.Type == "node":
			r := report.Workload{Kind: report.KindNode, ID: "node/" + x.Node, Name: x.Node, State: report.StateOK, Word: "Online",
				Facts: map[string]any{"cpus": int(x.MaxCPU), "memory_gib": gib(x.MaxMem)}}
			switch x.Status {
			case "online":
			case "offline":
				r.State, r.Word = report.StateDown, "Offline"
			default:
				r.State, r.Word = report.StateUnknown, "Its state is unknown to the cluster"
			}
			recs = append(recs, r)
			if r.State == report.StateOK {
				use = append(use, usage(r.ID, x))
			}
		case (x.Type == "qemu" || x.Type == "lxc") && x.Template == 0:
			r := guest(x, s.UUIDs[int(x.VMID)])
			recs = append(recs, r)
			if x.Status == "running" {
				use = append(use, usage(r.ID, x))
			}
		}
	}
	sort.SliceStable(recs[1:], func(i, j int) bool { return recs[1+i].ID < recs[1+j].ID })
	return recs, use
}

func clusterRecord(s *Snapshot) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: s.Cluster, State: report.StateOK, Word: "Quorate",
		Facts: map[string]any{"distribution": "Proxmox VE", "nodes": s.Nodes, "online": s.Online}}
	if s.Version != "" {
		r.Facts["version"] = s.Version
	}
	if !s.Clustered {
		r.Word = "A single node"
	}
	switch {
	case !s.Quorate:
		r.State, r.Word = report.StateDown, fmt.Sprintf("Not quorate: %d of %d nodes online", s.Online, s.Nodes)
	case len(s.Problems) > 0:
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

// guest is a VM (qemu) or a container (lxc). Proxmox lets two share a
// name, so the name is Proxmox's own "100 (db)", which also sorts as its
// tree does.
func guest(x resource, uuid string) report.Workload {
	vmid := int(x.VMID)
	name := fmt.Sprint(vmid)
	if x.Name != "" {
		name += " (" + x.Name + ")"
	}
	r := report.Workload{Kind: report.KindVM, ID: x.ID, Name: name, Node: x.Node, State: report.StateOK, Word: "Running",
		Facts: map[string]any{"vmid": vmid, "type": map[string]string{"qemu": "VM", "lxc": "container"}[x.Type],
			"vcpus": int(x.MaxCPU), "memory_gib": gib(x.MaxMem)}}
	if uuid != "" {
		r.Facts["firmware_uuid"] = uuid
	}
	if x.Pool != "" {
		r.Facts["pool"] = x.Pool
	}
	if x.Tags != "" {
		r.Facts["tags"] = strings.ReplaceAll(x.Tags, ";", ", ")
	}
	if x.HAState != "" {
		r.Facts["ha"] = x.HAState
	}
	if x.Lock != "" {
		r.Facts["lock"] = x.Lock
	}
	if x.Status != "running" {
		r.State, r.Word = report.StateStopped, "Stopped"
	}
	// HA says more than the status does, and a lock says what's under way.
	switch x.HAState {
	case "error":
		r.State, r.Word = report.StateDown, "HA can't run it (error)"
	case "fence", "recovery":
		r.State, r.Word = report.StateDown, "Its node failed: HA is recovering it ("+x.HAState+")"
	case "migrate", "relocate":
		r.State, r.Word = report.StateUpdating, "Moving to another node ("+x.HAState+")"
	case "started", "request_start":
		if x.Status != "running" {
			r.State, r.Word = report.StateStarting, "HA is starting it"
		}
	}
	if x.Lock == "migrate" && r.State == report.StateOK {
		r.State, r.Word = report.StateUpdating, "Migrating"
	}
	return r
}

func usage(id string, x resource) report.Usage {
	cpus := float64(x.MaxCPU)
	return report.Usage{ID: id, CPU: math.Round(float64(x.CPU)*cpus*1000) / 1000, CPUOf: cpus, Mem: int64(x.Mem), MemOf: int64(x.MaxMem)}
}

func gib(b num) float64 { return math.Round(float64(b)/(1<<30)*10) / 10 }

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Unreachable is the last records with the cluster's own record saying the
// API doesn't answer: what was seen stays, as last seen.
func Unreachable(last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "proxmox"}
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
