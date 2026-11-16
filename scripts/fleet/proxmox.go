package main

import (
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A Proxmox VE cluster, as a node's agent sends it (docs/cluster-watcher.md,
// step 7): one fleet host's agent reads it and sends it under its own name.
const (
	proxmoxCluster = "pve-prod"
	proxmoxNodes   = 8
	// proxmoxGuestEvery: host 3 in every 20 is the cluster's VM number i/20.
	proxmoxGuestEvery = 20
)

var pveRoles = []string{"erp-app", "erp-db", "web", "cache", "mail", "files", "build", "ci-runner", "dns", "vpn", "logs", "backup"}

func newProxmox(name, token string, guests int) *fakeCluster {
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, source: "proxmox"}
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	put(report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: proxmoxCluster, State: report.StateOK, Word: "Quorate",
		Since: now.Add(-40 * 24 * time.Hour), Facts: map[string]any{"distribution": "Proxmox VE", "version": "8.4.1", "nodes": proxmoxNodes, "online": proxmoxNodes}})
	for n := 1; n <= proxmoxNodes; n++ {
		put(report.Workload{Kind: report.KindNode, ID: fmt.Sprintf("node/pve-%02d", n), Name: fmt.Sprintf("pve-%02d", n), State: report.StateOK, Word: "Online",
			Since: now.Add(-time.Duration(20+n) * 24 * time.Hour), Facts: map[string]any{"cpus": 64, "memory_gib": 512.0}})
	}
	for k := range guests {
		vmid := 100 + k
		typ, kind := "qemu", "VM"
		if k%7 == 6 {
			typ, kind = "lxc", "container"
		}
		role := pveRoles[k%len(pveRoles)]
		r := report.Workload{Kind: report.KindVM, ID: fmt.Sprintf("%s/%d", typ, vmid), Name: fmt.Sprintf("%d (%s-%02d)", vmid, role, k/len(pveRoles)),
			Node: fmt.Sprintf("pve-%02d", k%proxmoxNodes+1), State: report.StateOK, Word: "Running", Since: now.Add(-time.Duration(1+k%30) * 24 * time.Hour),
			Facts: map[string]any{"vmid": vmid, "type": kind, "vcpus": 2 << (k % 3), "memory_gib": float64(int(4) << (k % 3)), "pool": []string{"erp", "web", "infra"}[k%3], "ha": "started"}}
		if typ == "qemu" {
			r.Facts["firmware_uuid"] = vmUUID("proxmox-"+proxmoxCluster, k)
		}
		switch {
		case k%23 == 22:
			r.State, r.Word, r.Facts["ha"] = report.StateStopped, "Stopped", "stopped"
		case k == 57:
			r.State, r.Word, r.Facts["lock"] = report.StateUpdating, "Migrating", "migrate"
		case k%50 == 9:
			r.Facts["lock"] = "backup"
		}
		put(r)
	}
	c.flap("lxc/141", spellPVEHA, func(r *report.Workload, since time.Time) { // scenario.go
		r.State, r.Word, r.Since, r.Facts["ha"] = report.StateDown, "HA can't run it (error)", since, "error"
	})
	return c
}
