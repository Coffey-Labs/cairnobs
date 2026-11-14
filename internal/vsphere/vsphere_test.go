package vsphere

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/cairnobs/cairnobs/internal/report"
)

// vcsim, govmomi's vCenter simulator: a datacenter with a cluster of
// three hosts and a standalone one, two VMs on each, one datastore.
func vcsim(t *testing.T, pin bool) (*Client, *govmomi.Client) {
	t.Helper()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(model.Remove)
	model.Service.TLS = new(tls.Config)
	srv := model.Service.NewServer()
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	pw, _ := srv.URL.User.Password()
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte(pw+"\n"), 0o600)
	cfg := Config{URL: "https://" + srv.URL.Host, Username: srv.URL.User.Username(), PasswordFile: filepath.Join(dir, "password")}
	if pin {
		cfg.Fingerprint = soap.ThumbprintSHA256(srv.Certificate())
	} else {
		ca, err := srv.CertificateFile()
		if err != nil {
			t.Fatal(err)
		}
		cfg.CAFile = ca
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := govmomi.NewClient(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	return c, admin
}

func TestRead(t *testing.T) {
	ctx := context.Background()
	c, admin := vcsim(t, false)
	defer c.Close(ctx)
	now := time.Now()
	s, err := c.Read(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Hosts) != 4 || len(s.VMs) != 4 || len(s.Stores) != 1 || s.Version == "" || len(s.Problems) != 0 {
		t.Fatalf("%d hosts, %d VMs, %d datastores, version %q, problems %v", len(s.Hosts), len(s.VMs), len(s.Stores), s.Version, s.Problems)
	}
	recs, use := Records(s)
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[r.Kind]++
		if r.Kind == report.KindVM && (r.Node == "" || r.Facts["firmware_uuid"] == "" || r.State != report.StateOK) {
			t.Errorf("VM %+v", r)
		}
	}
	if kinds[report.KindCluster] != 1 || kinds[report.KindNode] != 4 || kinds[report.KindVM] != 4 || kinds[report.KindPVC] != 1 {
		t.Errorf("kinds %v", kinds)
	}
	if recs[0].ID != report.ClusterSelf || recs[0].Facts["distribution"] != "vCenter" || recs[0].Name != "127.0.0.1" {
		t.Errorf("vCenter %+v", recs[0])
	}
	if len(use) != 8 {
		t.Errorf("usage for %d", len(use))
	}

	// Change things as an administrator would: a VM off, a host into
	// maintenance, a snapshot kept too long.
	fd := find.NewFinder(admin.Client, true)
	dc, _ := fd.DefaultDatacenter(ctx)
	fd.SetDatacenter(dc)
	vms, _ := fd.VirtualMachineList(ctx, "*")
	hosts, _ := fd.HostSystemList(ctx, "*")
	off, snapped := vms[0], vms[1]
	if task, err := off.PowerOff(ctx); err != nil || task.Wait(ctx) != nil {
		t.Fatalf("power off: %v", err)
	}
	if task, err := snapped.CreateSnapshot(ctx, "before-upgrade", "", false, false); err != nil || task.Wait(ctx) != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var maint string
	for _, h := range hosts {
		if strings.Contains(h.InventoryPath, "/DC0_C0/") {
			if task, err := h.EnterMaintenanceMode(ctx, 0, false, nil); err == nil && task.Wait(ctx) == nil {
				maint = h.Reference().Value
				break
			}
		}
	}
	s, err = c.Read(ctx, now.Add(10*24*time.Hour+time.Minute)) // ten days on
	if err != nil {
		t.Fatal(err)
	}
	recs, _ = Records(s)
	by := map[string]report.Workload{}
	for _, r := range recs {
		by[r.ID] = r
	}
	if r := by[off.Reference().Value]; r.State != report.StateStopped || r.Word != "Powered off" {
		t.Errorf("powered off: %+v", r)
	}
	if r := by[snapped.Reference().Value]; r.State != report.StateDegraded || !strings.Contains(r.Word, "snapshot 10 days old") {
		t.Errorf("old snapshot: %+v", r)
	}
	if r := by[maint]; maint == "" || r.State != report.StateUpdating || r.Word != "In maintenance mode" {
		t.Errorf("maintenance (%s): %+v", maint, r)
	}
}

func TestPinnedAndRelogin(t *testing.T) {
	ctx := context.Background()
	c, _ := vcsim(t, true)
	if _, err := c.Read(ctx, time.Now()); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	// The session lapses (vCenter's idle timeout): logged in again, once.
	c.Close(ctx)
	if _, err := c.Read(ctx, time.Now()); err != nil {
		t.Fatalf("after logout: %v", err)
	}
	// A wrong pin: refused.
	c.cfg.Fingerprint = strings.Repeat("AB:", 31) + "AB"
	c.c = nil
	if _, err := c.Read(ctx, time.Now()); err == nil {
		t.Error("a wrong fingerprint was accepted")
	}
}

func TestAlarmsAndNames(t *testing.T) {
	yes := true
	ref := func(v string) types.ManagedObjectReference {
		return types.ManagedObjectReference{Type: "Alarm", Value: v}
	}
	s := &Snapshot{Name: "vc1", At: time.Now(), Alarms: map[string]string{"alarm-1": "Host CPU usage", "alarm-2": "Datastore usage on disk"}}
	r := report.Workload{State: report.StateOK, Word: "Connected", Facts: map[string]any{}}
	alarmed(&r, []types.AlarmState{{Alarm: ref("alarm-1"), OverallStatus: types.ManagedEntityStatusRed}, {Alarm: ref("alarm-2"), OverallStatus: types.ManagedEntityStatusYellow}}, s.Alarms)
	if r.State != report.StateDegraded || r.Word != "vCenter alarm: Host CPU usage" || r.Facts["alarms"] != "Datastore usage on disk (yellow), Host CPU usage (red)" {
		t.Errorf("red: %+v", r)
	}
	r = report.Workload{State: report.StateOK, Facts: map[string]any{}}
	alarmed(&r, []types.AlarmState{{Alarm: ref("alarm-1"), OverallStatus: types.ManagedEntityStatusRed, Acknowledged: &yes}}, s.Alarms)
	if r.State != report.StateOK {
		t.Errorf("acknowledged: %+v", r)
	}
	if _, err := New(Config{URL: "http://vc", Username: "u", PasswordFile: "x"}); err == nil {
		t.Error("plain http accepted")
	}
}
