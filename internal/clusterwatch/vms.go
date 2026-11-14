package clusterwatch

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// KubeVirt VMs (docs/cluster-watcher.md, A VM is a VM, wherever it runs):
// one vm record for each VirtualMachine, with what its running instance
// (VMI), its newest live migration and its guest agent add. A VMI with no
// VirtualMachine is a VM of its own. The virt-launcher pod that runs a VMI
// isn't sent: the VM stands for it.

// vmParts is everything known about one VM beside the VirtualMachine.
type vmParts struct {
	vmi *virtualMachineInstance
	mig *vmiMigration // the newest migration of its VMI
	fs  *guestFS
}

func nsName(m meta) string { return m.Namespace + "/" + m.Name }

// vmRecords is a record for every VM and bare VMI.
func (b *builder) vmRecords(v view) []report.Workload {
	vms := each[virtualMachine](v, "virtualmachines")
	vmis := each[virtualMachineInstance](v, "virtualmachineinstances")
	if len(vms) == 0 && len(vmis) == 0 {
		return nil
	}
	parts := map[string]*vmParts{}
	part := func(k string) *vmParts {
		p := parts[k]
		if p == nil {
			p = &vmParts{}
			parts[k] = p
		}
		return p
	}
	for _, i := range vmis {
		part(nsName(i.Metadata)).vmi = i
	}
	for _, m := range each[vmiMigration](v, "virtualmachineinstancemigrations") {
		p := part(m.Metadata.Namespace + "/" + m.Spec.VMIName)
		if p.mig == nil || m.Metadata.CreationTimestamp.After(p.mig.Metadata.CreationTimestamp) ||
			(m.Metadata.CreationTimestamp.Equal(p.mig.Metadata.CreationTimestamp) && m.Metadata.Name > p.mig.Metadata.Name) {
			p.mig = m
		}
	}
	for k, o := range v["guestfs"] {
		if fs, ok := o.(*guestFS); ok {
			part(k).fs = fs
		}
	}
	var out []report.Workload
	named := map[string]bool{}
	for _, vm := range vms {
		named[nsName(vm.Metadata)] = true
		out = append(out, b.vm(vm, part(nsName(vm.Metadata))))
	}
	for _, i := range vmis {
		if !named[nsName(i.Metadata)] {
			out = append(out, b.bareVMI(i, part(nsName(i.Metadata))))
		}
	}
	return out
}

// vmStates is KubeVirt's printable status in our states.
var vmStates = map[string]struct{ state, word string }{
	"Running":                 {report.StateOK, "Running"},
	"Stopped":                 {report.StateStopped, "Stopped"},
	"Provisioning":            {report.StateStarting, "Provisioning its volumes"},
	"Starting":                {report.StateStarting, "Starting"},
	"WaitingForVolumeBinding": {report.StatePending, "Waiting for its volumes to bind"},
	"WaitingForReceiver":      {report.StatePending, "Waiting for a receiver"},
	"Paused":                  {report.StateDegraded, "Paused"},
	"Stopping":                {report.StateStopped, "Stopping"},
	"Terminating":             {report.StateStopped, "Terminating"},
	"Migrating":               {report.StateUpdating, "Live migrating"},
	"CrashLoopBackOff":        {report.StateCrashloop, "Restarting (CrashLoopBackOff)"},
	"ErrorUnschedulable":      {report.StateDown, "Can't be scheduled (ErrorUnschedulable)"},
	"ErrImagePull":            {report.StateDown, "Can't pull its disk image (ErrImagePull)"},
	"ImagePullBackOff":        {report.StateDown, "Can't pull its disk image (ImagePullBackOff)"},
	"ErrorPvcNotFound":        {report.StateDown, "A volume it needs is missing (ErrorPvcNotFound)"},
	"ErrorDataVolumeNotFound": {report.StateDown, "A DataVolume it needs is missing (ErrorDataVolumeNotFound)"},
	"DataVolumeError":         {report.StateDown, "A DataVolume it needs failed (DataVolumeError)"},
	"Unknown":                 {report.StateUnknown, "Unknown"},
	"":                        {report.StateUnknown, "No status yet"},
}

func (b *builder) vm(vm *virtualMachine, p *vmParts) report.Workload {
	r := b.rec(report.KindVM, vm.Metadata.Name, vm.Metadata)
	strategy := vm.strategy()
	r.Facts = map[string]any{"run_strategy": strategy}
	if u := vm.Spec.Template.Spec.Domain.Firmware.UUID; u != "" {
		r.Facts["firmware_uuid"] = strings.ToLower(u)
	}
	st, ok := vmStates[vm.Status.PrintableStatus]
	if !ok {
		st = vmStates["Unknown"]
		st.word = vm.Status.PrintableStatus
	}
	r.State, r.Word = st.state, st.word
	r.Since = vm.Metadata.CreationTimestamp
	if c := findCond(vm.Status.Conditions, "Ready"); c != nil && !c.LastTransitionTime.IsZero() {
		r.Since = c.LastTransitionTime
	}
	// Meant to be running all the time, and isn't: KubeVirt should have
	// started it again.
	if r.State == report.StateStopped && strategy == "Always" && vm.Metadata.DeletionTimestamp == nil {
		r.State, r.Word = report.StateDown, "Stopped, though its run strategy is Always"
	}
	if r.State == report.StateDown && vm.Status.PrintableStatus == "ErrorUnschedulable" {
		if c := findCond(vm.Status.Conditions, "PodScheduled"); c != nil && c.Status == "False" && c.Message != "" {
			// The scheduler's reason, without its note on preemption.
			msg, _, _ := strings.Cut(c.Message, " preemption:")
			r.Word += ": " + short(msg, 120)
		}
	}
	b.vmFacts(&r, p)
	return r
}

// bareVMI is a VMI nothing made: it's judged by its phase.
func (b *builder) bareVMI(i *virtualMachineInstance, p *vmParts) report.Workload {
	r := b.rec(report.KindVM, i.Metadata.Name, i.Metadata)
	r.Facts = map[string]any{"run_strategy": "None (a VMI with no VirtualMachine)"}
	r.Since = i.Metadata.CreationTimestamp
	switch i.Status.Phase {
	case "Running":
		r.State, r.Word = report.StateOK, "Running"
	case "Pending", "Scheduling":
		r.State, r.Word = report.StatePending, "Waiting to be scheduled"
	case "Scheduled":
		r.State, r.Word = report.StateStarting, "Starting"
	case "Succeeded":
		r.State, r.Word = report.StateStopped, "Shut down"
	case "Failed":
		r.State, r.Word = report.StateDown, "Failed"
		if i.Status.Reason != "" {
			r.Word += " (" + i.Status.Reason + ")"
		}
	default:
		r.State, r.Word = report.StateUnknown, "Unknown"
	}
	if c := findCond(i.Status.Conditions, "Ready"); c != nil && !c.LastTransitionTime.IsZero() {
		r.Since = c.LastTransitionTime
	}
	b.vmFacts(&r, p)
	return r
}

// vmFacts adds what the running instance, its migrations and its guest
// agent say.
func (b *builder) vmFacts(r *report.Workload, p *vmParts) {
	if i := p.vmi; i != nil {
		r.Node = i.Status.NodeName
		if u := i.Spec.Domain.Firmware.UUID; u != "" {
			r.Facts["firmware_uuid"] = strings.ToLower(u)
		}
		if os := i.Status.GuestOSInfo.PrettyName; os != "" {
			r.Facts["guest_os"] = os
		}
		var ips []string
		for _, n := range i.Status.Interfaces {
			for _, a := range append([]string{n.IPAddress}, n.IPAddresses...) {
				// Link-local addresses say nothing anyone can reach.
				if a != "" && !strings.HasPrefix(strings.ToLower(a), "fe80:") && !strings.HasPrefix(a, "169.254.") && !slices.Contains(ips, a) {
					ips = append(ips, a)
				}
			}
		}
		if len(ips) > 4 {
			ips = append(ips[:4], fmt.Sprintf("+%d more", len(ips)-4))
		}
		if len(ips) > 0 {
			r.Facts["ips"] = strings.Join(ips, ", ")
		}
		if i.Status.Phase == "Running" {
			r.Facts["agent_connected"] = i.agentConnected()
		}
		if m := i.Status.MigrationState; m != nil && !m.Completed && !m.Failed && m.TargetNode != "" && r.State == report.StateUpdating {
			r.Word = "Live migrating to " + m.TargetNode
		}
	}
	if m := p.mig; m != nil && m.Status.Phase == "Failed" {
		at := m.Metadata.CreationTimestamp
		for _, t := range m.Status.PhaseTransitionTimestamps {
			if t.Phase == "Failed" {
				at = t.Time
			}
		}
		r.Facts["migration_failed"] = at.UTC().Format(time.RFC3339)
		why := ""
		for _, c := range m.Status.Conditions {
			if c.Status == "True" && (c.Message != "" || c.Reason != "") {
				why = c.Reason
				if c.Message != "" {
					why = c.Message
				}
			}
		}
		if why != "" {
			r.Facts["migration_error"] = short(why, 160)
		}
	}
	if fs := p.fs; fs != nil && p.vmi != nil && p.vmi.Status.Phase == "Running" {
		var lines []string
		worst, worstAt := -1, ""
		// One file system mounted in several places (btrfs subvolumes, bind
		// mounts) is said once, at its shortest mount point.
		items := slices.Clone(fs.Items)
		slices.SortStableFunc(items, func(a, b guestFSItem) int { return len(a.MountPoint) - len(b.MountPoint) })
		type same struct {
			typ         string
			used, total int64
		}
		seen := map[same]bool{}
		for _, f := range items {
			if f.TotalBytes <= 0 || f.MountPoint == "" || seen[same{f.FileSystemType, f.UsedBytes, f.TotalBytes}] {
				continue
			}
			seen[same{f.FileSystemType, f.UsedBytes, f.TotalBytes}] = true
			pct := int(math.Round(float64(f.UsedBytes) * 100 / float64(f.TotalBytes)))
			lines = append(lines, fmt.Sprintf("%s %d%% of %s", f.MountPoint, pct, size(f.TotalBytes)))
			if pct > worst || (pct == worst && f.MountPoint < worstAt) {
				worst, worstAt = pct, f.MountPoint
			}
		}
		if worst >= 0 {
			slices.Sort(lines)
			r.Facts["filesystems"] = strings.Join(lines, "; ")
			r.Facts["fullest_mount"] = worstAt
			r.Facts["fullest_pct"] = worst
		}
	}
}

// size is bytes for people, in the units guests show: "20 GiB".
func size(b int64) string {
	const k = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f, u := float64(b), 0
	for f >= k && u < len(units)-1 {
		f /= k
		u++
	}
	if f >= 10 || u == 0 {
		return fmt.Sprintf("%.0f %s", f, units[u])
	}
	return fmt.Sprintf("%.1f %s", f, units[u])
}

// dvInProgress are DataVolume phases on the way to a filled volume.
var dvInProgress = map[string]string{
	"ImportScheduled": "Import scheduled", "ImportInProgress": "Importing",
	"CloneScheduled": "Clone scheduled", "CloneInProgress": "Cloning", "CSICloneInProgress": "Cloning",
	"SnapshotForSmartCloneInProgress": "Cloning", "CloneFromSnapshotSourceInProgress": "Cloning",
	"SmartClonePVCInProgress": "Cloning", "PrepClaimInProgress": "Preparing", "RebindInProgress": "Preparing",
	"ExpansionInProgress": "Expanding", "NamespaceTransferInProgress": "Moving",
	"UploadScheduled": "Upload scheduled", "UploadReady": "Waiting for an upload",
	"PendingPopulation": "Waiting to be filled", "WaitForFirstConsumer": "Waiting for a VM to use it",
}

// dataVolume adds what CDI says about filling a volume to its PVC's
// record: an import or clone that's failing, or one under way (a volume
// being filled isn't Pending, it's busy).
func dataVolumeState(r *report.Workload, dv *dataVolume) {
	st := dv.Status
	if r.Facts == nil {
		r.Facts = map[string]any{}
	}
	if st.Phase != "" {
		r.Facts["datavolume"] = st.Phase
	}
	running := findCond(st.Conditions, "Running")
	why := ""
	if running != nil && running.Status == "False" && running.Message != "" {
		why = ": " + short(running.Message, 140)
	}
	// CDI couldn't even make the claim (ErrClaimNotValid, …): no phase yet.
	var claimErr *condition
	for _, t := range []string{"Bound", "Ready"} {
		if c := findCond(st.Conditions, t); c != nil && c.Status == "False" && strings.HasPrefix(c.Reason, "Err") {
			claimErr = c
			break
		}
	}
	switch {
	case claimErr != nil:
		word := "Can't make its volume (" + claimErr.Reason + ")"
		if claimErr.Message != "" {
			word += ": " + short(claimErr.Message, 140)
		}
		r.State, r.Word, r.Since = report.StateDegraded, word, claimErr.LastTransitionTime
	case st.Phase == "Failed":
		r.State, r.Word = report.StateDegraded, "Import or clone failed"+why
		if running != nil && !running.LastTransitionTime.IsZero() {
			r.Since = running.LastTransitionTime
		}
	case running != nil && running.Status == "False" && running.Reason != "" && running.Reason != "Completed" && st.RestartCount > 0:
		r.State, r.Word = report.StateDegraded, fmt.Sprintf("Import or clone failing, restarted %d times", st.RestartCount)+why
		r.Since = running.LastTransitionTime
	case dvInProgress[st.Phase] != "" && r.State != report.StateOK && r.State != report.StateDown:
		word := dvInProgress[st.Phase]
		if st.Progress != "" && st.Progress != "N/A" {
			word += ", " + st.Progress
		}
		r.State, r.Word = report.StateStarting, word
	}
	if len(r.Facts) == 0 {
		r.Facts = nil
	}
}

// dvWorker is a pod CDI runs to fill a DataVolume's volume (importer,
// cloner, uploader): the volume's record says how that goes.
func dvWorker(p *pod, dvs map[string]*dataVolume) bool {
	o := p.Metadata.controllerRef()
	return o != nil && o.Kind == "PersistentVolumeClaim" && dvs[p.Metadata.Namespace+"/"+o.Name] != nil
}

// launcherPod is a virt-launcher pod: its VM stands for it.
func launcherPod(p *pod) bool {
	o := p.Metadata.controllerRef()
	return o != nil && o.Kind == "VirtualMachineInstance"
}
