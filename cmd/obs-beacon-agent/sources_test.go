package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/internal/flasharray"
	"github.com/cairnobs/cairnobs/internal/nutanix"
	"github.com/cairnobs/cairnobs/internal/proxmox"
	"github.com/cairnobs/cairnobs/internal/purerest"
	"github.com/cairnobs/cairnobs/internal/vsphere"
)

func TestPlatformSetups(t *testing.T) {
	ns, err := platformSetups(&config{})
	if err != nil || len(ns) != 0 {
		t.Errorf("none set: %v %v", ns, err)
	}
	if names := sourceNames(ns); names == nil || len(names) != 0 {
		t.Errorf("none set: names %#v, want [] (not nil)", names)
	}
	// One of each, named by default.
	ns, err = platformSetups(&config{Proxmox: &proxmox.Config{}, VSphere: &vsphere.Config{URL: "https://VCSA-01.example.org"},
		Nutanix: &nutanix.Config{URL: "https://pc-01.example.org:9440"}})
	if err != nil || !slices.Equal(sourceNames(ns), []string{"proxmox", "vcsa-01.example.org", "pc-01.example.org"}) {
		t.Errorf("three set: %v %v", sourceNames(ns), err)
	}
	// Two that would be one source.
	_, err = platformSetups(&config{VSphere: &vsphere.Config{URL: "https://mgmt"}, Nutanix: &nutanix.Config{URL: "https://pc", Name: "mgmt"}})
	if err == nil || !strings.Contains(err.Error(), `both called "mgmt"`) {
		t.Errorf("a shared name: %v", err)
	}
	// Arrays, any number, beside the platforms; two at one address clash.
	ns, err = platformSetups(&config{Nutanix: &nutanix.Config{URL: "https://pc"}, Pure: []flasharray.Config{{URL: "https://fa-01"}, {URL: "https://fa-02", Name: "dr"}}})
	if err != nil || !slices.Equal(sourceNames(ns), []string{"pc", "fa-01", "dr"}) {
		t.Errorf("arrays: %v %v", sourceNames(ns), err)
	}
	if _, err = platformSetups(&config{Pure: []flasharray.Config{{URL: "https://fa-01"}, {URL: "https://FA-01:443"}}}); err == nil {
		t.Error("one array twice")
	}
	// A FlashBlade is one of them, read as one.
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("t"), 0o600)
	ns, err = platformSetups(&config{Pure: []purerest.Config{{URL: "https://fa-01", TokenFile: tok}, {Type: "flashblade", URL: "https://fb-01", TokenFile: tok}}})
	if err != nil || len(ns) != 2 {
		t.Fatalf("a FlashArray and a FlashBlade: %v", err)
	}
	for i, want := range []string{"flasharray fa-01", "flashblade fb-01"} {
		if p, err := ns[i].setup(); err != nil || p.name != want {
			t.Errorf("%d: %v %v, want %s", i, p, err, want)
		}
	}
	if _, err = platformSetups(&config{Proxmox: &proxmox.Config{Name: "PVE prod"}}); err == nil {
		t.Error("a name with a space taken")
	}
}
