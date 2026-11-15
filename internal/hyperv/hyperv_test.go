package hyperv

import (
	"testing"

	"github.com/cairnobs/cairnobs/internal/report"
)

// What WMI said on a Server 2025 Hyper-V host, 2026-11-15.
func TestRecords(t *testing.T) {
	vms := []VM{
		{ID: "0591A50D-4CC0-4751-AAE8-5B6101247BA4", Name: "app01", State: 2, Health: 5, BIOSGUID: "{1FEF52CA-5E9F-4EF5-BEAD-3149ACD613D4}", Gen: 2, AutoStart: 3, VCPUs: 2, MemMB: 512},
		{ID: "289f1f7a-936a-4bee-8262-0ed2e05aca74", Name: "legacy01", State: 3, Health: 5, BIOSGUID: "A6B3A1A5-8776-4700-9ED6-48E6E9C395BA", Gen: 1, AutoStart: 2, VCPUs: 1, MemMB: 256},
		{ID: "447a4a03-d064-4292-8142-080cad59f64e", Name: "sql01", State: 2, Health: 25, Gen: 2, AutoStart: 4},
		{ID: "5a6b7c8d-0000-4000-8000-000000000001", Name: "saved01", State: 32769, Health: 5},
	}
	r := Records(vms)
	if len(r) != 4 {
		t.Fatalf("%d records", len(r))
	}
	a := r[0]
	if a.Kind != report.KindVM || a.ID != "0591a50d-4cc0-4751-aae8-5b6101247ba4" || a.State != report.StateOK || a.Word != "Running" ||
		a.Facts["firmware_uuid"] != "1fef52ca-5e9f-4ef5-bead-3149acd613d4" || a.Facts["hypervisor"] != "Hyper-V" ||
		a.Facts["vcpus"] != 2 || a.Facts["memory_gib"] != 0.5 || a.Facts["generation"] != 2 || a.Facts["autostart"] != true {
		t.Errorf("app01: %+v", a)
	}
	if l := r[1]; l.State != report.StateStopped || l.Word != "Off" || l.Facts["autostart"] != false || l.Facts["memory_gib"] != 0.3 {
		t.Errorf("legacy01: %+v", l)
	}
	if s := r[2]; s.State != report.StateDegraded || s.Word != "Running, but Hyper-V reports a failure" || s.Facts["firmware_uuid"] != nil {
		t.Errorf("sql01: %+v", s)
	}
	if s := r[3]; s.State != report.StateStopped || s.Word != "Saved" {
		t.Errorf("saved01: %+v", s)
	}
}

func TestIDs(t *testing.T) {
	for s, want := range map[string]bool{"0591a50d-4cc0-4751-aae8-5b6101247ba4": true, "WIN2025": false, "0591a50d-4cc0-4751-aae8-5b6101247bax": false, "": false} {
		if IsVMID(s) != want {
			t.Errorf("IsVMID(%q) = %v", s, !want)
		}
	}
	for in, want := range map[string]string{
		`Microsoft:0591A50D-4CC0-4751-AAE8-5B6101247BA4\b637f346-6a0e-4dec-af52-bd70cb80a21d\0`: "0591a50d-4cc0-4751-aae8-5b6101247ba4",
		`Microsoft:0591a50d-4cc0-4751-aae8-5b6101247ba4`:                                        "0591a50d-4cc0-4751-aae8-5b6101247ba4",
		`Microsoft:Definition\abc`:                                                              "",
		`Other:0591a50d-4cc0-4751-aae8-5b6101247ba4`:                                            "",
	} {
		if got := IDFromInstance(in); got != want {
			t.Errorf("IDFromInstance(%q) = %q", in, got)
		}
	}
}
