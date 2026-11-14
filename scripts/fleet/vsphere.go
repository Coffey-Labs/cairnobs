package main

import (
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A vCenter, as a host agent with a vsphere: section sends it
// (docs/cluster-watcher.md, step 8): fleet-0080's agent reads it.
const (
	vcenterName = "vcsa-01.example.org"
	esxiHosts   = 16
	vcenterHost = 80 // the fleet host whose agent reads it
	// vsphereGuestEvery: host 7 in every 20 is the vCenter's VM number
	// i/20; every other one on old virtual hardware (its UUID byte-swapped).
	vsphereGuestEvery = 20
)

var vmwareRoles = []string{"sql", "exchange", "sap-app", "sap-db", "citrix", "fileserver", "dc", "print", "jira", "jenkins", "splunk", "veeam"}

func newVSphere(name, token string, vms int) *fakeCluster {
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, source: vcenterName}
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	put(report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: vcenterName, State: report.StateOK, Word: "API answering",
		Since: now.Add(-60 * 24 * time.Hour), Facts: map[string]any{"distribution": "vCenter", "version": "8.0.3", "hosts": esxiHosts}})
	for n := 1; n <= esxiHosts; n++ {
		r := report.Workload{Kind: report.KindNode, ID: fmt.Sprintf("host-%d", 1000+n), Name: fmt.Sprintf("esxi-%02d.example.org", n),
			State: report.StateOK, Word: "Connected", Since: now.Add(-time.Duration(30+n) * 24 * time.Hour),
			Facts: map[string]any{"cpus": 48, "memory_gib": 768.0, "version": "VMware ESXi 8.0.3 build-24414501"}}
		switch n {
		case 9:
			r.State, r.Word, r.Since = report.StateUpdating, "In maintenance mode", now.Add(-35*time.Minute)
		case 13:
			r.State, r.Word, r.Facts["alarms"] = report.StateDegraded, "vCenter alarm: Host memory usage", "Host memory usage (red)"
		}
		put(r)
	}
	for d := 1; d <= 6; d++ {
		used := []float64{0, 41, 63, 91, 55, 72, 38}[d]
		r := report.Workload{Kind: report.KindPVC, ID: fmt.Sprintf("datastore-%d", 2000+d), Name: fmt.Sprintf("vsanDatastore-%02d", d),
			State: report.StateOK, Word: fmt.Sprintf("Accessible, %.0f%% full", used), Since: now.Add(-90 * 24 * time.Hour),
			Facts: map[string]any{"type": "vsan", "capacity_gib": 40960.0, "free_gib": 40960 * (100 - used) / 100, "used_pct": used}}
		if used >= 85 {
			r.State, r.Word = report.StateDegraded, fmt.Sprintf("%.0f%% full", used)
		}
		put(r)
	}
	for k := range vms {
		host := k%esxiHosts + 1
		if host == 9 { // in maintenance: its VMs moved off
			host = 10
		}
		r := report.Workload{Kind: report.KindVM, ID: fmt.Sprintf("vm-%d", 3000+k), Name: fmt.Sprintf("%s-%02d", vmwareRoles[k%len(vmwareRoles)], k/len(vmwareRoles)),
			Node: fmt.Sprintf("esxi-%02d.example.org", host), State: report.StateOK, Word: "Running", Since: now.Add(-time.Duration(2+k%40) * 24 * time.Hour),
			Facts: map[string]any{"hypervisor": "vSphere", "firmware_uuid": vmUUID("vsphere-"+vcenterName, k), "vcpus": 2 << (k % 3),
				"memory_gib": float64(int(8) << (k % 3)), "guest_os": []string{"Microsoft Windows Server 2022 (64-bit)", "Red Hat Enterprise Linux 9 (64-bit)"}[k%2]}}
		switch {
		case k%19 == 18:
			r.State, r.Word = report.StateStopped, "Powered off"
		case k == 77:
			r.State, r.Word, r.Facts["oldest_snapshot"] = report.StateDegraded, "Running, with a snapshot 23 days old", now.Add(-23*24*time.Hour).Format(time.DateOnly)
		case k == 150:
			r.State, r.Word = report.StateDown, "Inaccessible: its files can't be reached"
			r.Since = now.Add(-9 * time.Minute)
		case k%40 == 5:
			r.Facts["vmware_tools"] = "not running"
		}
		put(r)
	}
	return c
}
