package main

import (
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A Prism Central, as a host agent with a nutanix: section sends it
// (docs/cluster-watcher.md, step 9): fleet-0100's agent reads it.
const (
	prismCentral = "pc-01.example.org"
	ntnxClusters = 3
	ahvPerPE     = 6   // AHV hosts in each cluster
	nutanixHost  = 100 // the fleet host whose agent reads it
	// nutanixGuestEvery: host 11 in every 20 is Prism Central's VM number i/20.
	nutanixGuestEvery = 20
)

var ahvRoles = []string{"epic-app", "epic-db", "citrix-vda", "mssql", "oracle", "web", "ad", "backup-proxy", "k8s-worker", "elastic"}

func peName(c int) string { return fmt.Sprintf("pe-%s", []string{"phx-01", "phx-02", "tus-01"}[c]) }

func newNutanix(name, token string, vms int) *fakeCluster {
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, noUsage: true, source: prismCentral}
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	put(report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: prismCentral, State: report.StateOK, Word: "API answering",
		Since: now.Add(-45 * 24 * time.Hour), Facts: map[string]any{"distribution": "Prism Central", "version": "pc.2024.3.1",
			"hosts": ntnxClusters * ahvPerPE, "clusters": "pe-phx-01, pe-phx-02, pe-tus-01", "alerts": "pe-tus-01: NTP Not Synchronized (warning)"}})
	host := func(cl, n int) string { return fmt.Sprintf("ahv-%s-%02d", []string{"phx1", "phx2", "tus1"}[cl], n) }
	for cl := range ntnxClusters {
		for n := 1; n <= ahvPerPE; n++ {
			r := report.Workload{Kind: report.KindNode, ID: fmt.Sprintf("a1b2c3d4-%04d-4000-8000-%012d", cl, n), Name: host(cl, n),
				State: report.StateOK, Word: "Connected", Since: now.Add(-time.Duration(20+n) * 24 * time.Hour),
				Facts: map[string]any{"cluster": peName(cl), "cpus": 64, "memory_gib": 1024.0, "version": "AHV 10.0.1", "model": "NX-8170-G9"}}
			switch {
			case cl == 1 && n == 4:
				r.State, r.Word, r.Since = report.StateUpdating, "In maintenance mode", now.Add(-20*time.Minute)
			case cl == 2 && n == 2:
				r.State, r.Word, r.Facts["alerts"] = report.StateDegraded, "Alert from Prism: Disk Failure", "Disk Failure (critical)"
			}
			put(r)
		}
		for k, ct := range []string{"default-container", "vdi-images", "db-data"} {
			r := report.Workload{Kind: report.KindPVC, ID: fmt.Sprintf("c0ffee00-%04d-4000-8000-%012d", cl, k), Name: ct,
				State: report.StateOK, Word: "Available", Since: now.Add(-200 * 24 * time.Hour),
				Facts: map[string]any{"type": "storage container", "cluster": peName(cl), "capacity_gib": 81920.0, "replication_factor": 2}}
			if cl == 0 && k == 2 {
				r.State, r.Word, r.Facts["alerts"] = report.StateDegraded, "Alert from Prism: Storage Container Space Usage Exceeded", "Storage Container Space Usage Exceeded (critical)"
			}
			put(r)
		}
	}
	for k := range vms {
		cl, n := k%ntnxClusters, (k/ntnxClusters)%ahvPerPE+1
		if cl == 1 && n == 4 { // in maintenance: its VMs moved off
			n = 5
		}
		r := report.Workload{Kind: report.KindVM, ID: fmt.Sprintf("5f0e1d2c-%04d-4000-8000-%012d", cl, k), Name: fmt.Sprintf("%s-%02d", ahvRoles[k%len(ahvRoles)], k/len(ahvRoles)),
			Node: host(cl, n), State: report.StateOK, Word: "Running", Since: now.Add(-time.Duration(1+k%60) * 24 * time.Hour),
			Facts: map[string]any{"hypervisor": "AHV", "cluster": peName(cl), "firmware_uuid": vmUUID("nutanix-"+prismCentral, k), "vcpus": 4 << (k % 2),
				"memory_gib": float64(int(16) << (k % 3)), "guest_os": []string{"Red Hat Enterprise Linux 9.4", "Windows Server 2022 Datacenter"}[k%2]}}
		switch {
		case k%23 == 22:
			r.State, r.Word, r.Node = report.StateStopped, "Powered off", ""
		case k == 41:
			r.Facts["alerts"] = "VM Snapshot Is Old (warning)"
		case k%50 == 13:
			r.Facts["guest_tools"] = "not reachable"
		}
		put(r)
	}
	return c
}
