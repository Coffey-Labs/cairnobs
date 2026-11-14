package server

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A VM's full guest disk and a failed live migration are conditions of
// their own; a DataVolume that won't fill is its volume's.
func TestVMDisksMigrationsAndDataVolumes(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	recent := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	old := now.Add(-30 * time.Hour).UTC().Format(time.RFC3339)
	vm := func(id, name, state string, facts map[string]any) report.Workload {
		return report.Workload{Kind: report.KindVM, ID: id, Name: name, Namespace: "vms", State: state, Word: "Running", Facts: facts, Alert: report.AlertPage}
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: []report.Workload{
		vm("v1", "full", report.StateOK, map[string]any{"fullest_pct": 96, "fullest_mount": "/var", "filesystems": "/ 40% of 20 GiB; /var 96% of 50 GiB"}),
		vm("v2", "fullish", report.StateOK, map[string]any{"fullest_pct": 91, "fullest_mount": "/"}),
		vm("v3", "fine", report.StateOK, map[string]any{"fullest_pct": 89, "migration_failed": old}),
		vm("v4", "stuck", report.StateOK, map[string]any{"migration_failed": recent, "migration_error": "target node has no room"}),
		vm("v5", "off", report.StateStopped, map[string]any{"fullest_pct": 99, "migration_failed": recent}),
		{Kind: report.KindPVC, ID: "c1", Name: "img", Namespace: "vms", State: report.StateDegraded, Word: "Import or clone failed: 404", Alert: report.AlertPage},
	}})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	conds := a.workloadConditions(now)
	want := "warning:pvc/prod/vms/img critical:vmdisk/prod/vms/full warning:vmdisk/prod/vms/fullish warning:vmmigration/prod/vms/stuck"
	if got := keysOf(conds); got != want {
		t.Fatalf("conditions:\n got %s\nwant %s", got, want)
	}
	for _, c := range conds {
		switch c.Key {
		case "vmdisk/prod/vms/full":
			if c.Title != "VM full in vms (prod): /var is 96% full" || !strings.Contains(c.Detail, "/var 96% of 50 GiB") {
				t.Errorf("disk: %q %q", c.Title, c.Detail)
			}
		case "vmmigration/prod/vms/stuck":
			if !strings.HasPrefix(c.Detail, "target node has no room. ") {
				t.Errorf("migration: %q", c.Detail)
			}
		}
	}
}
