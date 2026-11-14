package vsphere

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/cairnobs/cairnobs/internal/report"
)

// snapshotOld is when a VM's oldest snapshot is worth a warning: snapshots
// grow, and slow the VM, the longer they're kept.
const snapshotOld = 7 * 24 * time.Hour

// Records makes a snapshot into the records a cluster agent sends: the
// vCenter itself, its ESXi hosts as nodes, its VMs, and its datastores as
// volumes, with what the running VMs and hosts use. Templates aren't VMs.
func Records(s *Snapshot) ([]report.Workload, []report.Usage) {
	recs := []report.Workload{vcenterRecord(s)}
	var use []report.Usage
	hostName := map[string]string{}
	mhz := map[string]float64{} // a host's core speed, for a VM's MHz in cores
	for _, h := range s.Hosts {
		hostName[h.Self.Value] = h.Name
		r := report.Workload{Kind: report.KindNode, ID: h.Self.Value, Name: h.Name, State: report.StateOK, Word: "Connected", Facts: map[string]any{}}
		if hw := h.Summary.Hardware; hw != nil {
			r.Facts["cpus"], r.Facts["memory_gib"] = int(hw.NumCpuCores), gib(hw.MemorySize)
		}
		if h.Summary.Config.Product != nil {
			r.Facts["version"] = h.Summary.Config.Product.FullName
		}
		switch {
		case h.Runtime.ConnectionState == types.HostSystemConnectionStateNotResponding:
			r.State, r.Word = report.StateDown, "Not responding"
		case h.Runtime.ConnectionState == types.HostSystemConnectionStateDisconnected:
			r.State, r.Word = report.StateDown, "Disconnected from the vCenter"
		case h.Runtime.InMaintenanceMode:
			r.State, r.Word = report.StateUpdating, "In maintenance mode"
		case h.Runtime.PowerState == types.HostSystemPowerStateStandBy:
			r.State, r.Word = report.StateStopped, "In standby"
		}
		alarmed(&r, h.TriggeredAlarmState, s.Alarms)
		recs = append(recs, r)
		if h.Summary.Hardware != nil {
			mhz[h.Self.Value] = float64(h.Summary.Hardware.CpuMhz)
		}
		if r.State == report.StateOK && h.Summary.Hardware != nil && h.Summary.Hardware.CpuMhz > 0 {
			hw := h.Summary.Hardware
			use = append(use, report.Usage{ID: r.ID, CPU: round3(float64(h.Summary.QuickStats.OverallCpuUsage) / float64(hw.CpuMhz)),
				CPUOf: float64(hw.NumCpuCores), Mem: int64(h.Summary.QuickStats.OverallMemoryUsage) << 20, MemOf: hw.MemorySize})
		}
	}
	// vSphere names are unique per folder, not per vCenter: a name two VMs
	// share gets its id, so each keeps its own history and alerts.
	count := map[string]int{}
	for _, v := range s.VMs {
		if v.Config != nil && !v.Config.Template {
			count[v.Name]++
		}
	}
	for _, v := range s.VMs {
		if v.Config == nil || v.Config.Template {
			continue // a template, or one whose config can't be read (inaccessible: counted below)
		}
		name := v.Name
		if count[name] > 1 {
			name += " (" + v.Self.Value + ")"
		}
		r := report.Workload{Kind: report.KindVM, ID: v.Self.Value, Name: name, State: report.StateOK, Word: "Running",
			Facts: map[string]any{"hypervisor": "vSphere", "vcpus": int(v.Config.Hardware.NumCPU), "memory_gib": round1(float64(v.Config.Hardware.MemoryMB) / 1024)}}
		if v.Runtime.Host != nil {
			r.Node = hostName[v.Runtime.Host.Value]
		}
		if v.Config.Uuid != "" {
			r.Facts["firmware_uuid"] = strings.ToLower(v.Config.Uuid)
		}
		if v.Config.GuestFullName != "" {
			r.Facts["guest_os"] = v.Config.GuestFullName
		}
		switch v.Runtime.PowerState {
		case types.VirtualMachinePowerStatePoweredOff:
			r.State, r.Word = report.StateStopped, "Powered off"
		case types.VirtualMachinePowerStateSuspended:
			r.State, r.Word = report.StateStopped, "Suspended"
		}
		switch v.Runtime.ConnectionState {
		case types.VirtualMachineConnectionStateInaccessible:
			r.State, r.Word = report.StateDown, "Inaccessible: its files can't be reached"
		case types.VirtualMachineConnectionStateOrphaned:
			r.State, r.Word = report.StateDown, "Orphaned: its host no longer has it"
		case types.VirtualMachineConnectionStateInvalid:
			r.State, r.Word = report.StateDown, "Invalid: its configuration can't be read"
		case types.VirtualMachineConnectionStateDisconnected:
			r.State, r.Word = report.StateUnknown, "Its host is disconnected"
		}
		if r.State == report.StateOK {
			if v.Guest != nil && v.Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning) {
				r.Facts["vmware_tools"] = "not running"
			}
			if at, ok := oldestSnapshot(v.Snapshot); ok {
				age := s.At.Sub(at)
				r.Facts["oldest_snapshot"] = at.UTC().Format(time.DateOnly)
				if age > snapshotOld {
					r.State, r.Word = report.StateDegraded, fmt.Sprintf("Running, with a snapshot %d days old", int(age.Hours()/24))
				}
			}
		}
		alarmed(&r, v.TriggeredAlarmState, s.Alarms)
		recs = append(recs, r)
		if v.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
			cores := 0.0
			if m := mhz[ref(v.Runtime.Host)]; m > 0 {
				cores = round3(float64(v.Summary.QuickStats.OverallCpuUsage) / m)
			}
			use = append(use, report.Usage{ID: r.ID, CPU: cores, CPUOf: float64(v.Config.Hardware.NumCPU),
				Mem: int64(v.Summary.QuickStats.HostMemoryUsage) << 20, MemOf: int64(v.Config.Hardware.MemoryMB) << 20})
		}
	}
	for _, d := range s.Stores {
		sum := d.Summary
		r := report.Workload{Kind: report.KindPVC, ID: d.Self.Value, Name: d.Name, State: report.StateOK, Word: "Accessible",
			Facts: map[string]any{"type": sum.Type, "capacity_gib": gib(sum.Capacity), "free_gib": gib(sum.FreeSpace)}}
		pct := 0.0
		if sum.Capacity > 0 {
			pct = 100 * float64(sum.Capacity-sum.FreeSpace) / float64(sum.Capacity)
			r.Facts["used_pct"] = math.Round(pct)
		}
		switch {
		case !sum.Accessible:
			r.State, r.Word = report.StateDown, "Not accessible"
		case pct >= 95:
			r.State, r.Word = report.StateDown, fmt.Sprintf("%.0f%% full", pct)
		case pct >= 85:
			r.State, r.Word = report.StateDegraded, fmt.Sprintf("%.0f%% full", pct)
		default:
			r.Word = fmt.Sprintf("Accessible, %.0f%% full", pct)
		}
		alarmed(&r, d.TriggeredAlarmState, s.Alarms)
		recs = append(recs, r)
	}
	sort.SliceStable(recs[1:], func(i, j int) bool { return recs[1+i].ID < recs[1+j].ID })
	return recs, use
}

func vcenterRecord(s *Snapshot) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: s.Name, State: report.StateOK, Word: "API answering",
		Facts: map[string]any{"distribution": "vCenter", "version": s.Version, "hosts": len(s.Hosts)}}
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

// alarmed notes vCenter's own triggered alarms. A red one nobody has
// acknowledged makes a record that's otherwise fine degraded: vCenter
// judged it, and says why.
func alarmed(r *report.Workload, states []types.AlarmState, names map[string]string) {
	var red, all []string
	for _, a := range states {
		n := names[a.Alarm.Value]
		if n == "" {
			n = a.Alarm.Value
		}
		all = append(all, fmt.Sprintf("%s (%s)", n, a.OverallStatus))
		if a.OverallStatus == types.ManagedEntityStatusRed && (a.Acknowledged == nil || !*a.Acknowledged) {
			red = append(red, n)
		}
	}
	if len(all) == 0 {
		return
	}
	sort.Strings(all)
	r.Facts["alarms"] = strings.Join(all, ", ")
	if len(red) > 0 && r.State == report.StateOK {
		sort.Strings(red)
		r.State, r.Word = report.StateDegraded, "vCenter alarm: "+strings.Join(red, ", ")
	}
}

func oldestSnapshot(info *types.VirtualMachineSnapshotInfo) (time.Time, bool) {
	if info == nil {
		return time.Time{}, false
	}
	var oldest time.Time
	var walk func([]types.VirtualMachineSnapshotTree)
	walk = func(ts []types.VirtualMachineSnapshotTree) {
		for _, t := range ts {
			if oldest.IsZero() || t.CreateTime.Before(oldest) {
				oldest = t.CreateTime
			}
			walk(t.ChildSnapshotList)
		}
	}
	walk(info.RootSnapshotList)
	return oldest, !oldest.IsZero()
}

func gib(b int64) float64      { return round1(float64(b) / (1 << 30)) }
func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func ref(r *types.ManagedObjectReference) string {
	if r == nil {
		return ""
	}
	return r.Value
}

// Unreachable is the last records with the vCenter's own record saying its
// API doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": "vCenter"}}
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
