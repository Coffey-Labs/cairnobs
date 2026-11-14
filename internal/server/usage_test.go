package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// A cluster agent's usage reading reaches every copy, shows on the
// Workloads list against its ceiling, and becomes hourly history.
func TestWorkloadUsage(t *testing.T) {
	a, b, _, clusterTok := clusterCopies(t)
	now := time.Now()
	at := now.Add(-20 * time.Second).Truncate(time.Second)
	sendBatch(t, a, clusterTok, report.Workloads{Full: report.FullBegin, Records: smallCluster(now)})
	sendBatch(t, a, clusterTok, report.Workloads{Full: report.FullEnd, UsageAt: at, Usage: []report.Usage{
		{ID: "n1", CPU: 2, Mem: 8 << 30, CPUOf: 4, MemOf: 16 << 30},
		{ID: "d1", CPU: 0.3, Mem: 600 << 20, MemOf: 1 << 30, Pods: 3},
		{ID: "gone", CPU: 1},
		{ID: report.ClusterSelf, IO: &report.IO{ReadOps: 1000, WriteOps: 500, ReadUs: 300, WriteUs: 100, ReadBps: 1e9, WriteBps: 5e8, Used: 40 << 40, UsedOf: 100 << 40}, Leases: ptr(int64(100))},
	}})
	b.syncWorkloads(false)

	cookie := post(b.Handler(), "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	var list struct{ Workloads []WorkloadRow }
	_ = json.Unmarshal(get(b.Handler(), "/api/v1/workloads?source=prod&limit=1000", cookie).Body.Bytes(), &list)
	got := map[string]*UsageNow{}
	for _, r := range list.Workloads {
		got[r.ID] = r.Usage
	}
	if u := got["n1"]; u == nil || u.CPUPct != 50 || u.MemPct != 50 || !u.At.Equal(at) {
		t.Errorf("node: %+v", u)
	}
	if u := got["d1"]; u == nil || u.CPUPct != 0 || u.MemPct != 58.6 {
		t.Errorf("deployment (no CPU limit): %+v", u)
	}
	if u := got["p1"]; u != nil {
		t.Errorf("a pod has no usage of its own: %+v", u)
	}
	// An array's performance is its own, on Clusters (docs/pure.md, step 4).
	var cl struct{ Clusters []ClusterView }
	_ = json.Unmarshal(get(b.Handler(), "/api/v1/clusters", cookie).Body.Bytes(), &cl)
	if len(cl.Clusters) != 1 || cl.Clusters[0].Usage == nil || cl.Clusters[0].Usage.IO == nil || cl.Clusters[0].Usage.IO.ReadOps != 1000 ||
		cl.Clusters[0].History != "prod" || cl.Clusters[0].Usage.Leases == nil || *cl.Clusters[0].Usage.Leases != 100 {
		t.Errorf("clusters: %+v", cl.Clusters)
	}

	// The recorder counts each reading once, and writes the hour as it ends.
	rec := &recorder{wl: map[string]*state.WorkloadHour{}, wlN: map[string]int{}, wlSeen: map[string]time.Time{}}
	hour := now.Truncate(time.Hour)
	b.sampleWorkloads(rec, hour, now)
	b.sampleWorkloads(rec, hour, now.Add(time.Minute)) // the same reading
	sendBatch(t, a, clusterTok, report.Workloads{UsageAt: at.Add(time.Minute), Usage: []report.Usage{{ID: "n1", CPU: 3, Mem: 10 << 30, CPUOf: 4, MemOf: 16 << 30},
		{ID: report.ClusterSelf, IO: &report.IO{ReadOps: 3000, WriteOps: 700, ReadUs: 500, WriteUs: 300, ReadBps: 3e9, WriteBps: 7e8, Used: 41 << 40, UsedOf: 100 << 40}, Leases: ptr(int64(300))}}})
	b.syncWorkloads(false)
	b.sampleWorkloads(rec, hour, now.Add(time.Minute))
	b.writeWorkloadHours(rec)
	hs, err := b.db.WorkloadHours("prod", "worker-1", hour, hour.Add(time.Hour))
	if err != nil || len(hs) != 1 || hs[0].CPU != 2.5 || hs[0].CPUMax != 3 || hs[0].Kind != report.KindNode || hs[0].CPUOf != 4 {
		t.Fatalf("node's hour: %+v %v", hs, err)
	}
	hs, _ = b.db.WorkloadHours("prod", "shop/deployment/web", hour, hour.Add(time.Hour))
	if len(hs) != 1 || hs[0].CPU != 0.3 {
		t.Errorf("deployment's hour: %+v", hs)
	}
	hs, _ = b.db.WorkloadHours("prod", "prod", hour, hour.Add(time.Hour))
	if len(hs) != 1 || hs[0].ReadOps != 2000 || hs[0].WriteOps != 600 || hs[0].ReadUs != 400 || hs[0].ReadUsMax != 500 || hs[0].WriteUsMax != 300 ||
		hs[0].ReadBps != 2e9 || hs[0].Used != 41<<40 || hs[0].UsedOf != 100<<40 || hs[0].Kind != report.KindCluster || hs[0].Leases != 200 || hs[0].LeasesMax != 300 {
		t.Errorf("the array's hour: %+v", hs)
	}
	var hours struct{ Hours []map[string]any }
	hr := get(b.Handler(), "/api/v1/workloads/hours?source=prod&workload=worker-1", cookie)
	_ = json.Unmarshal(hr.Body.Bytes(), &hours)
	if len(hours.Hours) != 1 {
		t.Errorf("hours API: %d %s", hr.Code, hr.Body)
	}
}

func ptr[T any](v T) *T { return &v }
