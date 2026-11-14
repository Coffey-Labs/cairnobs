package clusterwatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
	"github.com/cairnobs/cairnobs/internal/report"
)

// KubeVirt's VMs, as v1.x serves them, become vm records; their
// virt-launcher pods don't.
func TestVMRecords(t *testing.T) {
	v := view{}
	vm := func(uid, name, spec, status string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"vms","creationTimestamp":"` + t0 + `"},"spec":` + spec + `,"status":` + status + `}`
	}
	v.add(t, "virtualmachines", vm("v1", "db", `{"runStrategy":"Always","template":{"spec":{"domain":{"firmware":{"uuid":"5D307CA9-B3EF-428C-8861-06E72D69F223"}}}}}`,
		`{"printableStatus":"Running","conditions":[{"type":"Ready","status":"True","lastTransitionTime":"`+t0+`"}]}`))
	v.add(t, "virtualmachines", vm("v2", "spare", `{"running":false}`, `{"printableStatus":"Stopped"}`))
	v.add(t, "virtualmachines", vm("v3", "web", `{"runStrategy":"Always"}`, `{"printableStatus":"Stopped"}`))
	v.add(t, "virtualmachines", vm("v4", "big", `{"runStrategy":"Always"}`,
		`{"printableStatus":"ErrorUnschedulable","conditions":[{"type":"Ready","status":"False","reason":"GuestNotRunning","message":"Guest VM is not reported as running"},
		{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes are available: 3 Insufficient memory. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod."}]}`))
	v.add(t, "virtualmachines", vm("v5", "mover", `{"runStrategy":"Always"}`, `{"printableStatus":"Migrating"}`))
	v.add(t, "virtualmachines", vm("v6", "odd", `{"runStrategy":"Manual"}`, `{"printableStatus":"SomethingNew"}`))

	vmi := func(uid, name, status string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"vms","creationTimestamp":"` + t0 + `"},"spec":{"domain":{"firmware":{"uuid":"5d307ca9-b3ef-428c-8861-06e72d69f223"}}},"status":` + status + `}`
	}
	v.add(t, "virtualmachineinstances", vmi("i1", "db", `{"phase":"Running","nodeName":"worker-1",
		"conditions":[{"type":"Ready","status":"True"},{"type":"AgentConnected","status":"True"}],
		"guestOSInfo":{"prettyName":"Fedora Linux 41 (Cloud Edition)"},
		"interfaces":[{"ipAddress":"10.128.2.14","ipAddresses":["10.128.2.14","fd02::e","fe80::4064:d0ff:fe7a:a80a"]}]}`))
	v.add(t, "virtualmachineinstances", vmi("i5", "mover", `{"phase":"Running","nodeName":"worker-1","migrationState":{"targetNode":"worker-2"}}`))
	v.add(t, "virtualmachineinstances", vmi("i7", "scratch", `{"phase":"Failed","reason":"PodTerminating"}`))
	// Two migrations of db: the newer one failed.
	v.add(t, "virtualmachineinstancemigrations", `{"metadata":{"uid":"m1","name":"db-a","namespace":"vms","creationTimestamp":"2026-11-13T08:00:00Z"},"spec":{"vmiName":"db"},"status":{"phase":"Succeeded"}}`)
	v.add(t, "virtualmachineinstancemigrations", `{"metadata":{"uid":"m2","name":"db-b","namespace":"vms","creationTimestamp":"2026-11-13T08:30:00Z"},"spec":{"vmiName":"db"},
		"status":{"phase":"Failed","phaseTransitionTimestamps":[{"phase":"Running","phaseTransitionTimestamp":"2026-11-13T08:30:05Z"},{"phase":"Failed","phaseTransitionTimestamp":"2026-11-13T08:31:00Z"}]}}`)
	// btrfs subvolumes: one file system, three mount points.
	v["guestfs"] = map[string]any{"vms/db": &guestFS{Items: []guestFSItem{{"/var", "btrfs", 19 << 30, 20 << 30}, {"/", "btrfs", 19 << 30, 20 << 30},
		{"/home", "btrfs", 19 << 30, 20 << 30}, {"/boot", "ext4", 100 << 20, 1 << 30}, {"/mnt/cd", "iso9660", 0, 0}}}}
	// db's virt-launcher pod.
	v.add(t, "pods", `{"metadata":{"uid":"p1","name":"virt-launcher-db-x7k2p","namespace":"vms","ownerReferences":[{"kind":"VirtualMachineInstance","name":"db","controller":true}]},"status":{"phase":"Running"}}`)

	got := byName(build(v))
	for _, c := range []struct{ name, state, word string }{
		{"db", "ok", "Running"},
		{"spare", "stopped", "Stopped"},
		{"web", "down", "Stopped, though its run strategy is Always"},
		{"big", "down", "Can't be scheduled (ErrorUnschedulable): 0/3 nodes are available: 3 Insufficient memory."},
		{"mover", "updating", "Live migrating to worker-2"},
		{"odd", "unknown", "SomethingNew"},
		{"scratch", "down", "Failed (PodTerminating)"},
	} {
		r, ok := got["vm:"+c.name]
		if !ok || r.State != c.state || r.Word != c.word {
			t.Errorf("%s: %q %q, want %q %q", c.name, r.State, r.Word, c.state, c.word)
		}
	}
	db := got["vm:db"]
	want := map[string]any{"run_strategy": "Always", "firmware_uuid": "5d307ca9-b3ef-428c-8861-06e72d69f223", "guest_os": "Fedora Linux 41 (Cloud Edition)",
		"ips": "10.128.2.14, fd02::e", "agent_connected": true, "migration_failed": "2026-11-13T08:31:00Z",
		"filesystems": "/ 95% of 20 GiB; /boot 10% of 1.0 GiB", "fullest_mount": "/", "fullest_pct": 95}
	for k, w := range want {
		if db.Facts[k] != w {
			t.Errorf("db %s = %#v, want %#v", k, db.Facts[k], w)
		}
	}
	if db.Node != "worker-1" || db.Namespace != "vms" || db.Since.IsZero() {
		t.Errorf("db: %+v", db)
	}
	if _, ok := got["pod:virt-launcher-db-x7k2p"]; ok {
		t.Error("the virt-launcher pod was sent")
	}
	if got["vm:scratch"].Facts["run_strategy"] != "None (a VMI with no VirtualMachine)" {
		t.Errorf("bare VMI: %+v", got["vm:scratch"])
	}
}

// CDI's DataVolumes say how filling a volume goes, on the volume's record.
func TestDataVolumes(t *testing.T) {
	v := view{}
	claim := func(uid, name, phase string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"vms","creationTimestamp":"` + t0 + `"},"status":{"phase":"` + phase + `"}}`
	}
	dv := func(uid, name, status string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"vms","creationTimestamp":"` + t0 + `"},"status":` + status + `}`
	}
	v.add(t, "persistentvolumeclaims", claim("c1", "db-root", "Bound"))
	v.add(t, "datavolumes", dv("d1", "db-root", `{"phase":"Succeeded","progress":"100.0%"}`))
	v.add(t, "persistentvolumeclaims", claim("c2", "web-root", "Pending"))
	v.add(t, "datavolumes", dv("d2", "web-root", `{"phase":"ImportInProgress","progress":"45.3%"}`))
	v.add(t, "persistentvolumeclaims", claim("c3", "bad-root", "Pending"))
	v.add(t, "datavolumes", dv("d3", "bad-root", `{"phase":"ImportInProgress","restartCount":3,
		"conditions":[{"type":"Running","status":"False","reason":"Error","message":"Unable to connect to http data source: expected status code 200, got 404"}]}`))
	v.add(t, "datavolumes", dv("d4", "new-root", `{"phase":"Failed","conditions":[{"type":"Running","status":"False","reason":"Error","message":"certificate signed by unknown authority"}]}`))

	v.add(t, "datavolumes", dv("d5", "noclaim", `{"conditions":[{"type":"Bound","status":"False","reason":"ErrClaimNotValid","message":"no accessMode specified in StorageProfile standard"},
		{"type":"Ready","status":"False","reason":"ErrClaimNotValid"},{"type":"Running","status":"False"}]}`))

	// CDI's importer for bad-root, crashing: the volume's record says so.
	v.add(t, "pods", `{"metadata":{"uid":"p1","name":"importer-bad-root","namespace":"vms","ownerReferences":[{"kind":"PersistentVolumeClaim","name":"bad-root","controller":true}]},"status":{"phase":"Running"}}`)

	got := byName(build(v))
	if _, ok := got["pod:importer-bad-root"]; ok {
		t.Error("the importer pod was sent")
	}
	for _, c := range []struct{ name, state, word string }{
		{"noclaim", "degraded", "Can't make its volume (ErrClaimNotValid): no accessMode specified in StorageProfile standard"},
		{"db-root", "ok", "Bound"},
		{"web-root", "starting", "Importing, 45.3%"},
		{"bad-root", "degraded", "Import or clone failing, restarted 3 times: Unable to connect to http data source: expected status code 200, got 404"},
		{"new-root", "degraded", "Import or clone failed: certificate signed by unknown authority"},
	} {
		r, ok := got["pvc:"+c.name]
		if !ok || r.State != c.state || r.Word != c.word {
			t.Errorf("%s: %q %q, want %q %q", c.name, r.State, r.Word, c.state, c.word)
		}
	}
	if got["pvc:db-root"].Facts["datavolume"] != "Succeeded" {
		t.Errorf("db-root: %+v", got["pvc:db-root"])
	}
}

// fsAPI answers filesystemlist for some VMs and refuses or fails others.
type fsAPI struct {
	answers map[string]any // path → *guestFS, or an error
	asked   []string
}

func (f *fsAPI) Get(_ context.Context, path string, v any) error {
	f.asked = append(f.asked, path)
	switch a := f.answers[path].(type) {
	case error:
		return a
	case string:
		return json.Unmarshal([]byte(a), v)
	}
	return &kube.StatusError{Code: http.StatusNotFound}
}
func (f *fsAPI) List(context.Context, string, func(json.RawMessage) error) (string, error) {
	return "", errors.New("not here")
}
func (f *fsAPI) Watch(context.Context, string, string, time.Duration, func(string, json.RawMessage) error) (string, error) {
	return "", errors.New("not here")
}
func (f *fsAPI) Version(context.Context) (string, error) { return "v1.32.2", nil }

func TestGuestFileSystems(t *testing.T) {
	s := newStore()
	add := func(name, status string) {
		_, o, err := decoder(func(o *virtualMachineInstance) string { return o.Metadata.UID })(json.RawMessage(
			`{"metadata":{"uid":"` + name + `","name":"` + name + `","namespace":"vms"},"status":` + status + `}`))
		if err != nil {
			t.Fatal(err)
		}
		s.objs["virtualmachineinstances"][name] = o
	}
	agent := `"conditions":[{"type":"AgentConnected","status":"True"}]`
	add("db", `{"phase":"Running",`+agent+`}`)
	add("web", `{"phase":"Running",`+agent+`}`)
	add("noagent", `{"phase":"Running"}`)
	add("off", `{"phase":"Succeeded",`+agent+`}`)
	path := func(n string) string {
		return "/apis/subresources.kubevirt.io/v1/namespaces/vms/virtualmachineinstances/" + n + "/filesystemlist"
	}
	api := &fsAPI{answers: map[string]any{
		path("db"):  `{"items":[{"mountPoint":"/","fileSystemType":"xfs","usedBytes":10,"totalBytes":100}]}`,
		path("web"): errors.New("guest agent not responding"),
	}}
	readGuestFS(context.Background(), api, s)
	if len(api.asked) != 2 {
		t.Errorf("asked %v, want only the running VMs with a guest agent", api.asked)
	}
	fs := s.objs["guestfs"]
	if len(fs) != 1 || fs["vms/db"].(*guestFS).Items[0].TotalBytes != 100 {
		t.Errorf("kept %+v", fs)
	}
	if len(s.problem) != 0 {
		t.Errorf("problems %v", s.problem)
	}
	// Not granted: the cluster record says so.
	api = &fsAPI{answers: map[string]any{path("db"): &kube.StatusError{Code: http.StatusForbidden}}}
	readGuestFS(context.Background(), api, s)
	if r := clusterRecord("c", "", s.problem, ""); r.State != report.StateDegraded || !strings.Contains(r.Word, "guest file systems: not allowed") {
		t.Errorf("forbidden: %+v", r)
	}
}
