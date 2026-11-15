//go:build windows

package hyperv

import (
	"errors"
	"os"
	"testing"
)

// TestReadLive reads this machine's Hyper-V when HYPERV_LIVE=1 (on a
// Hyper-V host; docs/windows.md, step 8): every VM with a name, an ID and
// a BIOS GUID.
func TestReadLive(t *testing.T) {
	if os.Getenv("HYPERV_LIVE") != "1" {
		t.Skip("HYPERV_LIVE=1 reads this machine's Hyper-V")
	}
	vms, err := Read()
	if errors.Is(err, ErrAbsent) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vms {
		t.Logf("%+v", v)
		if v.Name == "" || !IsVMID(v.ID) || v.BIOSGUID == "" {
			t.Errorf("incomplete: %+v", v)
		}
	}
	for _, r := range Records(vms) {
		t.Logf("%s %s %s %v", r.Name, r.State, r.Word, r.Facts)
	}
}
