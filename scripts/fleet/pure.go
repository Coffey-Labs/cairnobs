package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// FlashArrays, as a host agent with a pure: section sends them
// (docs/pure.md): fleet-0040's agent reads them all, each a source of its
// own.
const pureHost = 40 // the fleet host whose agent reads them

// pureArrays are the arrays, in the order -pure N takes them.
var pureArrays = []string{"fa-prod-01", "fa-prod-02", "fa-dr-01", "fa-vdi-01", "fb-prod-01"}

// newPure is the k'th array: a FlashArray, or the FlashBlade last.
func newPure(name, token string, k int) *fakeCluster {
	if strings.HasPrefix(pureArrays[k], "fb-") {
		return newFlashBlade(name, token, k)
	}
	return newFlashArray(name, token, k, 600)
}

// newFlashBlade is fb-prod-01 (docs/pure.md, step 5): ten blades, one
// evacuating now and then (scenario.go); 400 file systems, cluster-02's
// first 100 VM disks among them (Portworx names them after their
// PersistentVolumes); 40 buckets.
func newFlashBlade(name, token string, k int) *fakeCluster {
	arr := pureArrays[k]
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, noUsage: true, source: arr}
	c.usageFn = func(now time.Time) []report.Usage { return pureIO(k, now) }
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	put(report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: arr, State: report.StateOK, Word: "API answering",
		Since: now.Add(-200 * 24 * time.Hour), Facts: map[string]any{"distribution": "Purity//FB", "version": "4.5.6", "model": "FlashBlade//S", "capacity_tib": 2000.0,
			"used_pct": 61.0, "data_reduction": 3.1}})
	for i := 1; i <= 10; i++ {
		put(report.Workload{Kind: report.KindNode, ID: fmt.Sprintf("ch1.fb%d", i), Name: fmt.Sprintf("CH1.FB%d", i), State: report.StateOK, Word: "Healthy",
			Since: now.Add(-200 * 24 * time.Hour), Facts: map[string]any{"raw_tib": 48.0}})
	}
	put(report.Workload{Kind: report.KindCluster, ID: "connection/c-fb-dr-01", Name: "connection/fb-dr-01", State: report.StateOK, Word: "Connected",
		Since: now.Add(-90 * 24 * time.Hour), Facts: map[string]any{"type": "async-replication", "version": "4.5.6"}})
	for n := range 400 {
		fname := fmt.Sprintf("share-%03d", n)
		if n < 100 {
			fname = "px_8f3a2b1c-" + pvName("cluster-02", n)
		}
		r := report.Workload{Kind: report.KindPVC, ID: fmt.Sprintf("%08x-0000-4000-8000-%012d", k, n), Name: fname, State: report.StateOK,
			Word: "Shared over NFSv4.1", Since: now.Add(-time.Duration(1+n%300) * 24 * time.Hour),
			Facts: map[string]any{"type": "file system", "provisioned_gib": float64(int(100) << (n % 5)), "used_gib": float64(20 + n%70), "protocols": "NFSv4.1"}}
		switch {
		case n == 100:
			r.Name, r.Word, r.Facts["protocols"] = "home", "Shared over NFSv3, NFSv4.1, SMB", "NFSv3, NFSv4.1, SMB"
			r.Facts["replication"] = "to fb-dr-01, replicating, 4 min behind"
		case n == 101:
			r.Name = "scratch"
		case n%50 == 7:
			r.Word = "Not shared over any protocol"
			delete(r.Facts, "protocols")
		}
		put(r)
	}
	for n := range 40 {
		r := report.Workload{Kind: report.KindPVC, ID: fmt.Sprintf("%08x-0001-4000-8000-%012d", k, n), Name: fmt.Sprintf("bucket-%02d", n), State: report.StateOK,
			Word: "Ready", Since: now.Add(-time.Duration(1+n*5) * 24 * time.Hour),
			Facts: map[string]any{"type": "bucket", "account": []string{"veeam", "ops", "web", "ml"}[n%4], "used_gib": float64(500 + n*37), "objects": 1000 * (n + 1)}}
		if n == 0 {
			r.Name, r.Facts["replication"], r.Facts["versioning"] = "backups", "to s3-offsite, paused", "enabled"
		}
		put(r)
	}
	c.flap("ch1.fb6", spellBladeEvac, func(r *report.Workload, since time.Time) {
		done := min(95, 5+int(time.Since(since).Minutes()))
		r.State, r.Word, r.Since = report.StateUpdating, fmt.Sprintf("Evacuating its data (%d%%)", done), since
	})
	return c
}

func newFlashArray(name, token string, k, volumes int) *fakeCluster {
	arr := pureArrays[k]
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, noUsage: true, source: arr}
	c.usageFn = func(now time.Time) []report.Usage { return pureIO(k, now) }
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: arr, State: report.StateOK, Word: "API answering",
		Since: now.Add(-120 * 24 * time.Hour), Facts: map[string]any{"distribution": "Purity//FA", "version": "6.7.3", "capacity_tib": 500.0,
			"used_pct": float64([]int{71, 68, 54, 83}[k]), "data_reduction": []float64{4.2, 4.1, 5.3, 9.8}[k], "model": "FA-X70R4"}}
	if arr == "fa-prod-01" {
		self.State, self.Word = report.StateDegraded, "Hardware: CH0.BAY7 failed (Drive reported a media error)"
		self.Facts["hardware"], self.Facts["alerts"] = "CH0.BAY7 failed (Drive reported a media error)", "CH0.BAY7: Drive failure (critical)"
		self.Since = now.Add(-3 * time.Hour)
	}
	put(self)
	for n, mode := range []string{"primary", "secondary"} {
		put(report.Workload{Kind: report.KindNode, ID: fmt.Sprintf("ct%d", n), Name: fmt.Sprintf("CT%d", n), State: report.StateOK, Word: "Ready, " + mode,
			Since: now.Add(-40 * 24 * time.Hour), Facts: map[string]any{"mode": mode, "model": "FA-X70R4", "version": "6.7.3"}})
	}
	switch arr {
	case "fa-prod-01", "fa-prod-02":
		peer := map[string]string{"fa-prod-01": "fa-prod-02", "fa-prod-02": "fa-prod-01"}[arr]
		put(report.Workload{Kind: report.KindCluster, ID: "pod/p-prod", Name: "pod/prod-pod", State: report.StateOK, Word: "In sync",
			Since: now.Add(-60 * 24 * time.Hour), Facts: map[string]any{"arrays": 2}})
		put(report.Workload{Kind: report.KindCluster, ID: "connection/c-" + peer, Name: "connection/" + peer, State: report.StateOK, Word: "Connected",
			Since: now.Add(-60 * 24 * time.Hour), Facts: map[string]any{"type": "sync-replication", "version": "6.7.3"}})
		put(report.Workload{Kind: report.KindCluster, ID: "connection/c-fa-dr-01", Name: "connection/fa-dr-01", State: report.StateOK, Word: "Connected",
			Since: now.Add(-60 * 24 * time.Hour), Facts: map[string]any{"type": "async-replication", "version": "6.7.3"}})
	case "fa-dr-01":
		put(report.Workload{Kind: report.KindCluster, ID: "connection/c-fa-prod-01", Name: "connection/fa-prod-01", State: report.StateOK,
			Word: "Connected", Since: now.Add(-60 * 24 * time.Hour), Facts: map[string]any{"type": "async-replication", "version": "6.7.3"}})
	}
	prefix := map[string]string{"fa-prod-01": "k8s-pvc", "fa-prod-02": "k8s-pvc", "fa-dr-01": "dr-copy", "fa-vdi-01": "vdi"}[arr]
	for n := range volumes {
		vname := fmt.Sprintf("%s-%04d", prefix, n)
		// cluster-00's VM disks are on fa-prod-01, cluster-01's in the pod
		// both prod arrays share: named after their PersistentVolumes.
		if cl := map[string]string{"fa-prod-01": "cluster-00", "fa-prod-02": "cluster-01"}[arr]; cl != "" && n < 300 {
			vname = "k8s-" + pvName(cl, n)
			if cl == "cluster-01" {
				vname = "prod-pod::" + vname
			}
		}
		r := report.Workload{Kind: report.KindPVC, ID: fmt.Sprintf("%08x-0000-4000-8000-%012d", k, n), Name: vname,
			State: report.StateOK, Word: "Connected", Since: now.Add(-time.Duration(1+n%200) * 24 * time.Hour),
			Facts: map[string]any{"type": "volume", "provisioned_gib": float64(int(50) << (n % 4)), "used_gib": float64(10 + n%40), "hosts": 1 + n%3}}
		switch {
		case arr == "fa-dr-01":
			r.Word = "Demoted: a replication target, read only"
		case n%97 == 13:
			r.Word, r.Facts["hosts"] = "Not connected to a host", 0
		case arr == "fa-prod-01" && n == 42:
			r.State, r.Word, r.Facts["alerts"] = report.StateDegraded, "Alert from Purity: Volume space exceeded", "Volume space exceeded (critical)"
		}
		if arr != "fa-vdi-01" && arr != "fa-dr-01" && n%5 == 0 {
			r.Facts["pod"] = "prod-pod"
		}
		put(r)
	}
	switch arr {
	case "fa-vdi-01":
		c.flap("ct1", spellVDIUpdate, func(r *report.Workload, since time.Time) {
			r.State, r.Word, r.Since = report.StateUpdating, "Updating", since
		})
	case "fa-dr-01":
		c.flap("connection/c-fa-prod-01", spellDRPaths, func(r *report.Workload, since time.Time) {
			r.State, r.Word, r.Since = report.StateDegraded, "Connected, but its paths aren't redundant", since
		})
	}
	return c
}

// pureIO is an array's performance a minute, as /arrays/performance gives
// it: busy in the working day, quiet at night, with noise; fa-prod-01's
// writes slow while its failed drive's data is rebuilt, and fa-dr-01 is
// written in bursts as replication lands.
func pureIO(k int, now time.Time) []report.Usage {
	base := []float64{62000, 48000, 9000, 31000, 140000}[k]
	day := 0.55 + 0.45*math.Sin(2*math.Pi*(float64(now.Hour()*60+now.Minute())/1440-0.3)) // peaks mid-afternoon
	noise := func() float64 { return 0.9 + 0.2*rand.Float64() }
	ops := base * day * noise()
	readShare := []float64{0.68, 0.7, 0.15, 0.8, 0.75}[k]
	readUs, writeUs := 250+60*day*noise(), 120+40*day*noise()
	if k == 0 {
		writeUs *= 2.4 // rebuilding
	}
	if k == 2 && now.Minute()%15 < 3 {
		ops *= 4 // a replication burst
	}
	capacity := int64(500) << 40
	if k == 4 {
		capacity = 2000 << 40 // the FlashBlade
		readUs, writeUs = readUs*3, writeUs*8
	}
	used := float64([]int{71, 68, 54, 83, 61}[k])/100 + float64(now.Unix()%86400)/86400*0.004 // grows a little through the day
	return []report.Usage{{ID: report.ClusterSelf, IO: &report.IO{
		ReadOps: math.Round(ops * readShare), WriteOps: math.Round(ops * (1 - readShare)), ReadUs: math.Round(readUs), WriteUs: math.Round(writeUs),
		ReadBps: math.Round(ops * readShare * 32 * 1024), WriteBps: math.Round(ops * (1 - readShare) * 24 * 1024),
		Used: int64(used * float64(capacity)), UsedOf: capacity}}}
}
