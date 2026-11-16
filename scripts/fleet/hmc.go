package main

import (
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/hmc"
	"github.com/cairnobs/cairnobs/internal/report"
)

// The HMC that manages the IBM i partitions' Power servers, as a host
// agent with an hmc: section sends it (docs/ibmi.md, step 4): the same
// agent as the partitions (fleet-0041) reads it, so -ibmi brings it too.
// The records are the reader's own, made from a snapshot as the HMC would
// answer.
const hmcName = "hmc1"

// newHMC is three servers: p10-prod runs the four IBM i partitions (their
// serial and partition numbers are the ones the partitions say), an AIX
// database and a Linux pair; p10-dr has its attention light on (a
// serviceable event nobody has closed); p9-old is in standby, waiting to
// be retired.
func newHMC(name, token string) *fakeCluster {
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, noUsage: true, source: hmcName}
	n := 0
	part := func(server, pname, typ, osv, state string, id int, units float64, vprocs int, memGiB int64) hmc.Partition {
		n++
		p := hmc.Partition{UUID: fmt.Sprintf("%08X-%04X-4C2E-9A1B-%012X", 0x5EED0000+n, id, n), ID: id, Name: pname, State: state, Type: typ, OS: osv,
			RMC: "active", Attention: "false", MemMB: memGiB << 10, Dedicated: "false", ProcUnits: units, VirtProcs: vprocs, SharedMode: "uncapped"}
		switch {
		case typ == "OS400":
			p.RMC, p.RefCode = "none", "00000000"
		case state != "running":
			p.RMC = "inactive"
		}
		return p
	}
	vios := func(server string, k, id int) hmc.Partition {
		return part(server, fmt.Sprintf("%s-vios%d", server, k), "Virtual IO Server", "VIOS 4.1.1.10", "running", id, 1, 2, 16)
	}
	ibmiOS := "IBM i Licensed Internal Code 7.5"
	prod := hmc.Server{UUID: "3c0d6a1e-7f5b-3a2c-9d41-0e2f6b8a1c11", Name: "p10-prod", State: "operating", DetailedState: "None",
		MachineType: "9105", Model: "42A", Serial: "78ABC12", ProcUnits: 40, ProcFree: 9.5, MemMB: 2 << 20, MemFreeMB: 393_216, AttentionLED: "false",
		Partitions: []hmc.Partition{vios("p10-prod", 1, 1), vios("p10-prod", 2, 6),
			part("p10-prod", "PROD1", "OS400", ibmiOS, "running", 2, 4, 8, 256), part("p10-prod", "PROD2", "OS400", ibmiOS, "running", 3, 3, 6, 192),
			part("p10-prod", "HA1", "OS400", ibmiOS, "running", 4, 2, 4, 128), part("p10-prod", "DEV1", "OS400", "IBM i Licensed Internal Code 7.4", "running", 5, 1, 2, 64),
			part("p10-prod", "aixdb1", "AIX/Linux", "AIX 7.3 7300-03-00-2446", "running", 7, 6, 8, 512),
			part("p10-prod", "lnxweb1", "AIX/Linux", "Linux/Red Hat 5.14.0-503.11.1.el9_5.ppc64le 9.5", "running", 8, 1.5, 4, 32),
			part("p10-prod", "lnxweb2", "AIX/Linux", "Linux/Red Hat 5.14.0-503.11.1.el9_5.ppc64le 9.5", "running", 9, 1.5, 4, 32)}}
	dr := hmc.Server{UUID: "8e41b2d7-2a6c-3f90-b5e3-71c4d0a9f622", Name: "p10-dr", State: "operating", DetailedState: "None",
		MachineType: "9105", Model: "22B", Serial: "78DEF45", ProcUnits: 20, ProcFree: 12.25, MemMB: 1 << 20, MemFreeMB: 655_360, AttentionLED: "true",
		Partitions: []hmc.Partition{vios("p10-dr", 1, 1), vios("p10-dr", 2, 2),
			part("p10-dr", "aixdb1-dr", "AIX/Linux", "AIX 7.3 7300-03-00-2446", "running", 3, 2, 8, 512),
			part("p10-dr", "lnxtest1", "AIX/Linux", "Linux/SUSE 6.4.0-150600.23.25-default 15.6", "running", 4, 0.5, 2, 16),
			part("p10-dr", "aixtest1", "AIX/Linux", "Unknown", "not activated", 5, 0.5, 1, 16)}}
	old := hmc.Server{UUID: "1f7a9c3e-5b2d-3e84-a6c0-94d2e8b1f733", Name: "p9-old", State: "standby", DetailedState: "None",
		MachineType: "9009", Model: "42A", Serial: "13XYZ99", ProcUnits: 16, ProcFree: 16, MemMB: 512 << 10, MemFreeMB: 512 << 10, AttentionLED: "false",
		Partitions: []hmc.Partition{part("p9-old", "aixold1", "AIX/Linux", "AIX 7.2 7200-05-08-2420", "not activated", 1, 2, 4, 64)}}
	s := &hmc.Snapshot{Name: hmcName, Console: hmcName, Servers: []hmc.Server{prod, dr, old}, Problems: map[string]string{}, At: time.Now()}
	now := time.Now()
	for i, r := range hmc.Records(s) {
		r.Since = now.Add(-time.Duration(30+i%11) * 24 * time.Hour)
		c.recs[r.ID] = r
	}
	return c
}
