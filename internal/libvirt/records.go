package libvirt

import (
	"math"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Records makes the host's domains into vm records, the same shape a
// cluster's KubeVirt VMs take, and works out what the running ones use
// since prev, the previous reading (by UUID; nil the first time).
func Records(doms []Domain, prev map[string]Domain) ([]report.Workload, []report.Usage) {
	recs := make([]report.Workload, 0, len(doms))
	var use []report.Usage
	for _, d := range doms {
		r := report.Workload{Kind: report.KindVM, ID: d.UUID, Name: d.Name}
		r.State, r.Word = stateOf(d.State, d.Reason)
		r.Facts = map[string]any{"firmware_uuid": d.UUID, "autostart": d.Autostart, "hypervisor": "libvirt"}
		if d.VCPUs > 0 {
			r.Facts["vcpus"] = d.VCPUs
		}
		if d.MaxKiB > 0 {
			r.Facts["memory_gib"] = math.Round(float64(d.MaxKiB)/(1<<20)*10) / 10
		}
		recs = append(recs, r)
		p, ok := prev[d.UUID]
		if !ok || !running(d.State) || d.CPUNs < p.CPUNs || d.Read <= p.Read {
			continue
		}
		u := report.Usage{ID: d.UUID, CPUOf: float64(d.VCPUs), MemOf: int64(d.MaxKiB) * 1024}
		u.CPU = math.Round(float64(d.CPUNs-p.CPUNs)/1e9/(float64(d.Read-p.Read)/1000)*1000) / 1000
		u.Mem = int64(d.RSSKiB) * 1024
		if u.Mem == 0 {
			u.Mem = int64(d.MemKiB) * 1024
		}
		use = append(use, u)
	}
	return recs, use
}

func running(state int) bool { return state == 1 || state == 2 || state == 3 }

// stateOf says a virDomainState and its reason (libvirt-domain.h) in our
// states: a crash or a disk error is down, a migration an update.
func stateOf(state, reason int) (string, string) {
	switch state {
	case 1, 2: // running, blocked on a resource
		return report.StateOK, "Running"
	case 3: // paused
		switch reason {
		case 2:
			return report.StateUpdating, "Migrating"
		case 5:
			return report.StateDown, "Paused by a disk I/O error"
		case 6:
			return report.StateDown, "Paused by its watchdog"
		case 10:
			return report.StateDown, "Paused after its guest crashed"
		case 11:
			return report.StateStarting, "Starting"
		}
		return report.StateDegraded, "Paused"
	case 4: // shutting down
		return report.StateStopped, "Shutting down"
	case 5: // shut off
		switch reason {
		case 3:
			return report.StateDown, "Crashed"
		case 6:
			return report.StateDown, "Failed to start"
		}
		return report.StateStopped, "Shut off"
	case 6:
		return report.StateDown, "Crashed"
	case 7:
		return report.StateStopped, "Suspended by its guest"
	}
	return report.StateUnknown, "No state"
}
