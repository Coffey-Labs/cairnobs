//go:build windows

package hyperv

import (
	"errors"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// Namespace is Hyper-V's WMI namespace (v2, Windows Server 2012 and later).
const Namespace = `root\virtualization\v2`

// Read is the host's VMs. ErrAbsent when Hyper-V isn't installed.
func Read() ([]VM, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	done, err := coInit()
	if err != nil {
		return nil, err
	}
	defer done()
	svc, err := connect(Namespace)
	if err != nil {
		var he *hresultError
		if errors.As(err, &he) && he.hr == wbemEInvalidNamespace {
			return nil, ErrAbsent
		}
		return nil, explain(err)
	}
	defer release(svc)
	// Hyper-V shows its VMs only to Administrators and Hyper-V
	// Administrators: anyone else gets an empty list, which would read as
	// "no VMs". Say so instead.
	if !allowed() {
		return nil, ErrNotAllowed
	}

	systems, err := query(svc, "SELECT Name, ElementName, EnabledState, HealthState, OnTimeInMilliseconds FROM Msvm_ComputerSystem",
		[]string{"Name", "ElementName", "EnabledState", "HealthState", "OnTimeInMilliseconds"})
	if err != nil {
		return nil, explain(err)
	}
	byID := map[string]*VM{}
	var order []string
	for _, r := range systems {
		id := strings.ToLower(r.str("Name"))
		if !IsVMID(id) { // the host itself is in the same class
			continue
		}
		byID[id] = &VM{ID: id, Name: r.str("ElementName"), State: int(r.int("EnabledState")), Health: int(r.int("HealthState")), UpMs: uint64(r.int("OnTimeInMilliseconds"))}
		order = append(order, id)
	}
	if len(byID) == 0 {
		return []VM{}, nil
	}
	// Each VM's current settings: its BIOS GUID, generation and start action.
	settings, err := query(svc, `SELECT VirtualSystemIdentifier, BIOSGUID, VirtualSystemSubType, AutomaticStartupAction FROM Msvm_VirtualSystemSettingData WHERE VirtualSystemType = "Microsoft:Hyper-V:System:Realized"`,
		[]string{"VirtualSystemIdentifier", "BIOSGUID", "VirtualSystemSubType", "AutomaticStartupAction"})
	if err != nil {
		return nil, explain(err)
	}
	for _, r := range settings {
		if v := byID[strings.ToLower(r.str("VirtualSystemIdentifier"))]; v != nil {
			v.BIOSGUID, v.AutoStart = r.str("BIOSGUID"), int(r.int("AutomaticStartupAction"))
			switch r.str("VirtualSystemSubType") {
			case "Microsoft:Hyper-V:SubType:1":
				v.Gen = 1
			case "Microsoft:Hyper-V:SubType:2":
				v.Gen = 2
			}
		}
	}
	// Processors and memory: one settings object a VM (checkpoints have
	// their own, under their own IDs, and are skipped).
	cpus, err := query(svc, "SELECT InstanceID, VirtualQuantity FROM Msvm_ProcessorSettingData", []string{"InstanceID", "VirtualQuantity"})
	if err == nil {
		for _, r := range cpus {
			if v := byID[IDFromInstance(r.str("InstanceID"))]; v != nil {
				v.VCPUs = int(r.int("VirtualQuantity"))
			}
		}
	}
	mem, err := query(svc, "SELECT InstanceID, VirtualQuantity FROM Msvm_MemorySettingData", []string{"InstanceID", "VirtualQuantity"})
	if err == nil {
		for _, r := range mem {
			if v := byID[IDFromInstance(r.str("InstanceID"))]; v != nil {
				v.MemMB = r.int("VirtualQuantity") // MB
			}
		}
	}
	out := make([]VM, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// allowed says this process may see Hyper-V's VMs: it's in
// Administrators (elevated) or Hyper-V Administrators.
func allowed() bool {
	// BUILTIN\Administrators and BUILTIN\Hyper-V Administrators.
	for _, id := range []string{"S-1-5-32-544", "S-1-5-32-578"} {
		sid, err := windows.StringToSid(id)
		if err != nil {
			continue
		}
		if ok, err := windows.Token(0).IsMember(sid); err == nil && ok {
			return true
		}
	}
	return false
}

// explain says what a denial means.
func explain(err error) error {
	var he *hresultError
	if errors.As(err, &he) && (he.hr == wbemEAccessDenied || he.hr == eAccessDenied) {
		return errors.New("not allowed to read Hyper-V's VMs (" + err.Error() + "): docs/windows.md, step 8, says what the agent's account needs")
	}
	return err
}
