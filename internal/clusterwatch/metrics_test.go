package clusterwatch

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
	"github.com/cairnobs/cairnobs/internal/report"
)

func TestQuantity(t *testing.T) {
	for q, want := range map[string]float64{
		"250m": 0.25, "2": 2, "123456789n": 0.123456789, "1500u": 0.0015, "512Mi": 512 << 20, "1.5Gi": 1.5 * (1 << 30),
		"16Gi": 16 << 30, "1k": 1000, "2M": 2e6, "1e3": 1000, "128974848": 128974848, "64Ki": 65536, "1E": 1e18,
	} {
		if got, ok := quantity(q); !ok || math.Abs(got-want) > want*1e-12 {
			t.Errorf("%s: %v %v, want %v", q, got, ok, want)
		}
	}
	for _, q := range []string{"", "Mi", "lots"} {
		if _, ok := quantity(q); ok {
			t.Errorf("%q read as a quantity", q)
		}
	}
}

// metricsAPI answers metrics.k8s.io from fixed JSON, or with an error.
type metricsAPI struct {
	fsAPI
	nodes, pods string
	err         error
}

func (m *metricsAPI) Get(_ context.Context, path string, v any) error {
	if m.err != nil {
		return m.err
	}
	js := map[string]string{"/apis/metrics.k8s.io/v1beta1/nodes": m.nodes, "/apis/metrics.k8s.io/v1beta1/pods": m.pods}[path]
	return json.Unmarshal([]byte(js), v)
}

// A node's usage is against its allocatable; a Deployment's is its pods'
// sum, against their limits; a VM's is its launcher pod's. A controller
// with a pod that has no limit has no ceiling; a bare pod has no record.
func TestUsage(t *testing.T) {
	s := newStore()
	v := s.objs
	v.add(t, "nodes", `{"metadata":{"uid":"n1","name":"w1"},"status":{"allocatable":{"cpu":"3500m","memory":"15Gi"}}}`)
	v.add(t, "deployments", `{"metadata":{"uid":"d1","name":"web","namespace":"shop"},"spec":{"replicas":2},"status":{"replicas":2,"readyReplicas":2}}`)
	v.add(t, "replicasets", `{"metadata":{"uid":"rs1","name":"web-1","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"web","controller":true}]}}`)
	v.add(t, "statefulsets", `{"metadata":{"uid":"s1","name":"db","namespace":"shop"},"spec":{"replicas":1},"status":{"replicas":1,"readyReplicas":1}}`)
	limited := `"spec":{"containers":[{"resources":{"limits":{"cpu":"500m","memory":"512Mi"}}},{"resources":{"limits":{"cpu":"100m","memory":"64Mi"}}}]}`
	v.add(t, "pods", `{"metadata":{"uid":"p1","name":"web-1-a","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"web-1","controller":true}]},`+limited+`,"status":{"phase":"Running"}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p2","name":"web-1-b","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"web-1","controller":true}]},`+limited+`,"status":{"phase":"Running"}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p3","name":"db-0","namespace":"shop","ownerReferences":[{"kind":"StatefulSet","name":"db","controller":true}]},
		"spec":{"containers":[{"resources":{"limits":{"memory":"1Gi"}}}]},"status":{"phase":"Running"}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p4","name":"debug","namespace":"shop"},"status":{"phase":"Running"}}`)
	v.add(t, "virtualmachines", `{"metadata":{"uid":"v1","name":"fedora","namespace":"vms"},"spec":{"runStrategy":"Always"},"status":{"printableStatus":"Running"}}`)
	v.add(t, "virtualmachineinstances", `{"metadata":{"uid":"i1","name":"fedora","namespace":"vms"},"status":{"phase":"Running","nodeName":"w1"}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p5","name":"virt-launcher-fedora-x","namespace":"vms","ownerReferences":[{"kind":"VirtualMachineInstance","name":"fedora","controller":true}]},
		"spec":{"containers":[{"resources":{"limits":{"cpu":"2","memory":"2Gi"}}}]},"status":{"phase":"Running"}}`)
	for _, r := range resources {
		s.synced[r.name] = true
	}
	api := &metricsAPI{
		nodes: `{"items":[{"metadata":{"name":"w1"},"usage":{"cpu":"1250000000n","memory":"6Gi"}}]}`,
		pods: `{"items":[
			{"metadata":{"name":"web-1-a","namespace":"shop"},"containers":[{"usage":{"cpu":"200m","memory":"300Mi"}},{"usage":{"cpu":"10m","memory":"20Mi"}}]},
			{"metadata":{"name":"web-1-b","namespace":"shop"},"containers":[{"usage":{"cpu":"150m","memory":"280Mi"}},{"usage":{"cpu":"5m","memory":"20Mi"}}]},
			{"metadata":{"name":"db-0","namespace":"shop"},"containers":[{"usage":{"cpu":"40m","memory":"700Mi"}}]},
			{"metadata":{"name":"debug","namespace":"shop"},"containers":[{"usage":{"cpu":"1m","memory":"5Mi"}}]},
			{"metadata":{"name":"virt-launcher-fedora-x","namespace":"vms"},"containers":[{"usage":{"cpu":"600m","memory":"1800Mi"}}]}]}`,
	}
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	readMetrics(context.Background(), api, s, at)
	if s.usageAt != at {
		t.Fatalf("usage read at %v", s.usageAt)
	}
	view := s.snapshot()
	got := map[string]report.Usage{}
	for _, u := range usage(view, build(view)) {
		got[u.ID] = u
	}
	want := map[string]report.Usage{
		"n1": {ID: "n1", CPU: 1.25, Mem: 6 << 30, CPUOf: 3.5, MemOf: 15 << 30},
		"d1": {ID: "d1", CPU: 0.365, Mem: 620 << 20, CPUOf: 1.2, MemOf: 1152 << 20, Pods: 2},
		"s1": {ID: "s1", CPU: 0.04, Mem: 700 << 20, MemOf: 1 << 30, Pods: 1}, // no CPU limit: no CPU ceiling
		"v1": {ID: "v1", CPU: 0.6, Mem: 1800 << 20, CPUOf: 2, MemOf: 2 << 30, Pods: 1},
	}
	if len(got) != len(want) {
		t.Errorf("usage for %d records, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		if g := got[id]; g != w {
			t.Errorf("%s: %+v, want %+v", id, g, w)
		}
	}

	// No metrics-server: no usage, and nothing wrong.
	readMetrics(context.Background(), &metricsAPI{err: &kube.StatusError{Code: http.StatusNotFound}}, s, at.Add(time.Minute))
	if !s.usageAt.IsZero() || s.problem[metricsRes] != "" || usage(s.snapshot(), nil) != nil {
		t.Errorf("not served: usage at %v, problem %q", s.usageAt, s.problem[metricsRes])
	}
	// Not allowed: the cluster's record says so.
	readMetrics(context.Background(), &metricsAPI{err: &kube.StatusError{Code: http.StatusForbidden}}, s, at.Add(2*time.Minute))
	if s.problem[metricsRes] == "" {
		t.Error("a refusal isn't a problem")
	}
}

// Each reading goes to obs-beacond once, with the next batch, even when
// nothing else changed; a failed send keeps it for the next.
func TestSenderSendsUsageOnce(t *testing.T) {
	s := newStore()
	s.objs.add(t, "nodes", `{"metadata":{"uid":"n1","name":"w1"},"status":{"allocatable":{"cpu":"4","memory":"16Gi"}}}`)
	for _, r := range resources {
		s.synced[r.name] = true
	}
	out := &sink{}
	sn := newSender(Config{Name: "prod"}, out.send)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	sn.tick(context.Background(), s, now) // the full listing, before any reading
	out.take()

	api := &metricsAPI{nodes: `{"items":[{"metadata":{"name":"w1"},"usage":{"cpu":"1","memory":"4Gi"}}]}`, pods: `{"items":[]}`}
	readMetrics(context.Background(), api, s, now.Add(time.Second))
	out.fail = true
	sn.tick(context.Background(), s, now.Add(5*time.Second))
	out.fail = false
	sn.tick(context.Background(), s, now.Add(10*time.Second))
	got := out.take()
	if len(got) != 1 || len(got[0].Usage) != 1 || got[0].Usage[0].CPU != 1 || !got[0].UsageAt.Equal(now.Add(time.Second)) {
		t.Fatalf("after a reading: %+v", got)
	}
	sn.tick(context.Background(), s, now.Add(15*time.Second))
	if got := out.take(); len(got) != 0 {
		t.Errorf("the same reading again: %+v", got)
	}
}
