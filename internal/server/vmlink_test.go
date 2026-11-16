package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A host whose SMBIOS UUID is a cluster VM's firmware UUID is that VM: the
// pages say so both ways, and the VM or its node being down explains the
// host going silent.
func TestVMLinkedToItsHost(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	const uuid = "5d307ca9-b3ef-428c-8861-06e72d69f223"
	// node1 reported 10 minutes ago, as that VM: it's silent now.
	a.store.Put("node1", report.Report{V: report.Version, Host: "node1", Time: now.Add(-10 * time.Minute), Machine: strings.ToUpper(uuid)}, now.Add(-10*time.Minute))
	vm := func(state, word string) report.Workload {
		return report.Workload{Kind: report.KindVM, ID: "v1", Name: "db", Namespace: "vms", Node: "worker-1", State: state, Word: word,
			Alert: report.AlertPage, Facts: map[string]any{"firmware_uuid": uuid}}
	}
	node := func(state string) report.Workload {
		return report.Workload{Kind: report.KindNode, ID: "n1", Name: "worker-1", State: state, Word: state}
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: []report.Workload{node(report.StateOK), vm(report.StateOK, "Running")}})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	never := func(string) bool { return false }

	// Running: linked both ways; silence is the guest's or the network's.
	hosts, conds := a.view(now)
	if hosts[0].VM == nil || hosts[0].VM.Name != "db" || hosts[0].VM.Source != "prod" || hosts[0].VM.Href != "/workloads?source=prod&ns=vms&q=db" || hosts[0].VM.Platform != "Kubernetes" {
		t.Fatalf("host's VM: %+v", hosts[0].VM)
	}
	silent := find(conds, "silent/node1")
	if silent == nil || !strings.Contains(silent.Detail, "It's VM db in vms (prod), which is running as far as the hypervisor knows") {
		t.Fatalf("silent: %+v", silent)
	}
	rec := get(a.Handler(), "/api/v1/workloads?kind=vm", devCookie(t, a.Handler()))
	var list struct{ Workloads []WorkloadRow }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || len(list.Workloads) != 1 || list.Workloads[0].Host == nil || list.Workloads[0].Host.Name != "node1" {
		t.Fatalf("VM's host: %d %s", rec.Code, rec.Body)
	}
	if list.Workloads[0].Platform != "Kubernetes" {
		t.Errorf("VM's platform: %q", list.Workloads[0].Platform)
	}

	// The VM is down: one incident for it, the host held under it.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{vm(report.StateDown, "Can't be scheduled")}})
	_, conds = a.view(now)
	g := a.groupConditions(conds, never)
	if got := keysOf(g); got != "critical:vm/prod/vms/db" {
		t.Fatalf("VM down: %s", got)
	}
	if g[0].Affected != "node1" || !strings.Contains(g[0].Detail, "Also affected: node1 (1 alert held back).") {
		t.Fatalf("VM down: %q %q", g[0].Affected, g[0].Detail)
	}

	// Stopped, which nobody said it shouldn't be: no VM condition, but the
	// host's says why.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{vm(report.StateStopped, "Stopped")}})
	_, conds = a.view(now)
	if s := find(conds, "silent/node1"); s == nil || !strings.HasSuffix(s.Detail, "It's VM db in vms (prod), which is stopped.") {
		t.Fatalf("stopped: %+v", s)
	}

	// Its node goes down: one incident for the node, with the host.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{node(report.StateDown), vm(report.StateOK, "Running")}})
	_, conds = a.view(now)
	g = a.groupConditions(conds, never)
	if got := keysOf(g); got != "critical:node/prod/worker-1" || g[0].Affected != "node1" {
		t.Fatalf("node down: %s %q", got, g[0].Affected)
	}

	// Another host claims the same UUID (a clone): no link at all.
	a.store.Put("node2", report.Report{V: report.Version, Host: "node2", Time: now, Machine: uuid}, now)
	if byHost, byVM := a.vmLinks(); len(byHost) != 0 || len(byVM) != 0 {
		t.Fatalf("duplicate UUID linked: %v %v", byHost, byVM)
	}
}

func TestMachineUUID(t *testing.T) {
	for in, want := range map[string]string{
		"5D307CA9-B3EF-428C-8861-06E72D69F223\n": "5d307ca9-b3ef-428c-8861-06e72d69f223",
		"03000200-0400-0500-0006-000700080009":   "",
		"00000000-0000-0000-0000-000000000000":   "",
		"not a uuid":                             "",
		"":                                       "",
	} {
		if got := report.MachineUUID(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func find(conds []Condition, key string) *Condition {
	for i := range conds {
		if conds[i].Key == key {
			return &conds[i]
		}
	}
	return nil
}

// A host running libvirt reports its VMs: they're a source of their own,
// judged like a cluster's, linked to the guests' agents, and grouped
// under the host they run on when it goes silent.
func TestHostVMs(t *testing.T) {
	a, _, _, _ := clusterCopies(t)
	now := time.Now()
	const guest = "23a01893-0000-4000-8000-000000000001"
	vm := func(id, name, state, word string) report.Workload {
		return report.Workload{Kind: report.KindVM, ID: id, Name: name, State: state, Word: word,
			Facts: map[string]any{"firmware_uuid": id, "hypervisor": "libvirt"}}
	}
	kvm := func(at time.Time, vms ...report.Workload) {
		a.store.Put("kvm1", report.Report{V: report.Version, Host: "kvm1", Time: at, VMs: &report.VMs{List: vms,
			Usage: []report.Usage{{ID: guest, CPU: 0.5, Mem: 1 << 30, CPUOf: 2, MemOf: 4 << 30}}}}, at)
	}
	kvm(now, vm(guest, "web", report.StateOK, "Running"), vm("00000000-0000-4000-8000-0000000000bb", "spare", report.StateStopped, "Shut off"))
	a.store.Put("web", report.Report{V: report.Version, Host: "web", Time: now, Machine: guest}, now)

	rec := get(a.Handler(), "/api/v1/workloads?source=kvm1", devCookie(t, a.Handler()))
	var list struct {
		Workloads   []WorkloadRow
		Hypervisors []string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || len(list.Workloads) != 2 || len(list.Hypervisors) != 1 || list.Hypervisors[0] != "kvm1" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	for _, w := range list.Workloads {
		if w.Platform != "KVM" {
			t.Errorf("%s: platform %q, want KVM", w.Name, w.Platform)
		}
	}
	// ?platform= keeps one platform's, and the counts say how many each has.
	var by struct {
		Workloads  []WorkloadRow
		Platforms  map[string]int
		PlatformOf map[string]string `json:"platform_of"`
	}
	_ = json.Unmarshal(get(a.Handler(), "/api/v1/workloads?platform=KVM", devCookie(t, a.Handler())).Body.Bytes(), &by)
	if len(by.Workloads) != 2 || by.Platforms["KVM"] != 2 || by.PlatformOf["kvm1"] != "KVM" {
		t.Errorf("platform=KVM: %d rows, %v, %v", len(by.Workloads), by.Platforms, by.PlatformOf)
	}
	_ = json.Unmarshal(get(a.Handler(), "/api/v1/workloads?platform=Hyper-V", devCookie(t, a.Handler())).Body.Bytes(), &by)
	if len(by.Workloads) != 0 || by.Platforms["KVM"] != 2 {
		t.Errorf("platform=Hyper-V: %d rows, %v", len(by.Workloads), by.Platforms)
	}
	hosts, _ := a.view(now)
	for _, h := range hosts {
		if h.Name == "web" && (h.VM == nil || h.VM.Platform != "KVM") {
			t.Errorf("web's VM: %+v", h.VM)
		}
	}
	for _, w := range list.Workloads {
		if w.Name == "web" && (w.Host == nil || w.Host.Name != "web" || w.Usage == nil || w.Usage.CPU != 0.5) {
			t.Errorf("web: host %+v usage %+v", w.Host, w.Usage)
		}
	}
	hosts, conds := a.view(now)
	for _, h := range hosts {
		if h.Name == "web" && (h.VM == nil || h.VM.Source != "kvm1" || h.VM.Href != "/workloads?source=kvm1&q=web") {
			t.Errorf("guest's VM: %+v", h.VM)
		}
	}
	if c := find(conds, "vm/kvm1//spare"); c != nil {
		t.Errorf("a VM shut off is a condition: %+v", c)
	}
	for _, v := range a.clusterViews(now, conds) {
		if v.Name == "kvm1" {
			t.Error("a hypervisor among the clusters")
		}
	}

	// It crashes: a condition, a warning until it's adopted.
	kvm(now, vm(guest, "web", report.StateDown, "Crashed"))
	_, conds = a.view(now)
	if c := find(conds, "vm/kvm1//web"); c == nil || c.Severity != "warning" || !strings.Contains(c.Title, "web") {
		t.Fatalf("crashed: %+v", c)
	}

	// The hypervisor goes silent, and so does the guest: one incident, the
	// hypervisor's, with the guest under it; the VM is as last seen.
	then := now.Add(-10 * time.Minute)
	kvm(then, vm(guest, "web", report.StateOK, "Running"))
	a.store.Put("web", report.Report{V: report.Version, Host: "web", Time: then, Machine: guest}, then)
	_, conds = a.view(now)
	if find(conds, "vm/kvm1//web") != nil || find(conds, "silent/kvm1") == nil || find(conds, "silent/web") == nil {
		t.Fatalf("silent: %s", keysOf(conds))
	}
	g := a.groupConditions(conds, func(string) bool { return false })
	if got := keysOf(g); got != "critical:silent/kvm1" || !strings.Contains(g[0].Affected, "web") {
		t.Fatalf("grouped: %s %q", got, g[0].Affected)
	}

	// libvirt there but not answering: said once, on the host.
	a.store.Put("kvm1", report.Report{V: report.Version, Host: "kvm1", Time: now, VMs: &report.VMs{Error: "libvirt: permission denied"}}, now)
	_, conds = a.view(now)
	if c := find(conds, "vms/kvm1"); c == nil || c.Host != "kvm1" || !strings.Contains(c.Detail, "permission denied") {
		t.Fatalf("can't read: %+v", c)
	}
}

// A vSphere VM on older virtual hardware: its guest reads the BIOS UUID
// with the first three fields byte-swapped, and it's still the same machine.
// Only vSphere's VMs are matched that way.
func TestVSphereSwappedUUID(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	const bios = "4221a2b3-c4d5-e6f7-8899-aabbccddeeff"
	a.store.Put("node1", report.Report{V: report.Version, Host: "node1", Time: now, Machine: report.SwappedUUID(bios)}, now)
	vm := func(hv string) report.Workload {
		return report.Workload{Kind: report.KindVM, ID: "vm-42", Name: "erp", State: report.StateOK, Word: "Running",
			Facts: map[string]any{"firmware_uuid": bios, "hypervisor": hv}}
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: []report.Workload{vm("vSphere")}})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	if byHost, _ := a.vmLinks(); byHost["node1"].Name != "erp" {
		t.Fatalf("not linked: %+v", byHost)
	}
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{vm("KubeVirt")}})
	if byHost, _ := a.vmLinks(); len(byHost) != 0 {
		t.Fatalf("a KubeVirt VM matched swapped: %+v", byHost)
	}
}
