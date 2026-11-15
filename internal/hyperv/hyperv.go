// Package hyperv reads a Windows host's Hyper-V VMs for the agent
// (docs/windows.md, step 8): what they are and their state, as libvirt's
// are read on Linux, so a Hyper-V host's guests show on its page and an
// agent inside one links to its VM by the BIOS GUID. Read-only, through
// WMI's root\virtualization\v2; the reading is Windows' (read_windows.go),
// making records of it is plain Go.
package hyperv

import (
	"errors"
	"math"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// ErrAbsent is a host without Hyper-V: no VMs, and nothing wrong.
var ErrAbsent = errors.New("Hyper-V isn't installed here")

// ErrNotAllowed is an agent whose account can't see the VMs: Hyper-V
// shows them only to Administrators and Hyper-V Administrators, and the
// agent's own account is neither unless someone adds it (docs/windows.md,
// step 8).
var ErrNotAllowed = errors.New(`Hyper-V is here, but shows its VMs only to Administrators and Hyper-V Administrators, and this agent's account is in neither (docs/windows.md, step 8)`)

// VM is one VM as WMI says it.
type VM struct {
	ID        string // VMId: Msvm_ComputerSystem.Name, a GUID
	Name      string // ElementName
	State     int    // EnabledState
	Health    int    // HealthState: 5 OK, 20 major failure, 25 critical failure
	UpMs      uint64 // OnTimeInMilliseconds
	BIOSGUID  string // Msvm_VirtualSystemSettingData.BIOSGUID: the SMBIOS UUID the guest sees
	Gen       int    // 1 or 2, from VirtualSystemSubType
	AutoStart int    // AutomaticStartupAction: 2 none, 3 if it was running, 4 always
	VCPUs     int
	MemMB     int64
}

// Records makes the VMs into vm records, the shape libvirt's and a
// cluster's take.
func Records(vms []VM) []report.Workload {
	out := make([]report.Workload, 0, len(vms))
	for _, v := range vms {
		r := report.Workload{Kind: report.KindVM, ID: strings.ToLower(v.ID), Name: v.Name}
		r.State, r.Word = stateOf(v.State, v.Health)
		r.Facts = map[string]any{"hypervisor": "Hyper-V", "autostart": v.AutoStart == 3 || v.AutoStart == 4}
		if u := strings.ToLower(strings.Trim(v.BIOSGUID, "{}")); u != "" {
			r.Facts["firmware_uuid"] = u
		}
		if v.Gen > 0 {
			r.Facts["generation"] = v.Gen
		}
		if v.VCPUs > 0 {
			r.Facts["vcpus"] = v.VCPUs
		}
		if v.MemMB > 0 {
			r.Facts["memory_gib"] = math.Round(float64(v.MemMB)/1024*10) / 10
		}
		out = append(out, r)
	}
	return out
}

// Running says the VM is on.
func Running(state int) bool { return state == 2 }

// stateOf says an EnabledState (CIM_EnabledLogicalElement, with Hyper-V's
// own values) and HealthState in our states.
func stateOf(state, health int) (string, string) {
	if health == 20 || health == 25 {
		if state == 2 {
			return report.StateDegraded, "Running, but Hyper-V reports a failure"
		}
		return report.StateDown, "Hyper-V reports a failure"
	}
	switch state {
	case 2:
		return report.StateOK, "Running"
	case 3:
		return report.StateStopped, "Off"
	case 6, 32769:
		return report.StateStopped, "Saved"
	case 9, 32768:
		return report.StateDegraded, "Paused"
	case 32770, 32777:
		return report.StateStarting, "Starting"
	case 32771:
		return report.StateOK, "Running (taking a checkpoint)"
	case 32773, 32776:
		return report.StateStopped, "Saving"
	case 32774, 4:
		return report.StateStopped, "Stopping"
	case 32779:
		return report.StateUpdating, "Moving"
	}
	return report.StateUnknown, "No state"
}

// IsVMID says a Msvm_ComputerSystem's Name is a VM's GUID, not the host's
// own name (the host is listed in the same class).
func IsVMID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// IDFromInstance is the VM a settings object belongs to, from its
// InstanceID ("Microsoft:<VMId>\<setting>" or "Microsoft:<VMId>").
func IDFromInstance(instanceID string) string {
	s, ok := strings.CutPrefix(instanceID, "Microsoft:")
	if !ok {
		return ""
	}
	s, _, _ = strings.Cut(s, `\`)
	if !IsVMID(s) {
		return ""
	}
	return strings.ToLower(s)
}
