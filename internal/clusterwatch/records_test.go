package clusterwatch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// obj decodes a Kubernetes object, as the watch would, into the view.
func (v view) add(t *testing.T, res, js string) {
	t.Helper()
	for _, r := range resources {
		if r.name == res {
			id, o, err := r.decode(json.RawMessage(js))
			if err != nil {
				t.Fatalf("%s: %v", res, err)
			}
			if v[res] == nil {
				v[res] = map[string]any{}
			}
			v[res][id] = o
			return
		}
	}
	t.Fatalf("no resource %s", res)
}

func byName(recs []report.Workload) map[string]report.Workload {
	m := map[string]report.Workload{}
	for _, r := range recs {
		m[r.Kind+":"+r.Name] = r
	}
	return m
}

const t0 = "2026-11-13T09:00:00Z"

func TestRecordsFromObjects(t *testing.T) {
	v := view{}
	v.add(t, "namespaces", `{"metadata":{"uid":"ns1","name":"shop","annotations":{"obs.cairnobs.org/alert":"page"}}}`)
	v.add(t, "namespaces", `{"metadata":{"uid":"ns2","name":"batch"}}`)
	// Nodes: ready, not responding, cordoned and rebooting, under pressure.
	node := func(uid, name, ready, extra string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","labels":{"node-role.kubernetes.io/worker":""}},` + extra +
			`"status":{"nodeInfo":{"kubeletVersion":"v1.32.2"},"allocatable":{"cpu":"4","memory":"16Gi"},"conditions":[` + ready + `]}}`
	}
	v.add(t, "nodes", node("n1", "w1", `{"type":"Ready","status":"True","lastTransitionTime":"`+t0+`"}`, ""))
	v.add(t, "nodes", node("n2", "w2", `{"type":"Ready","status":"Unknown","reason":"NodeStatusUnknown","lastTransitionTime":"`+t0+`"}`, ""))
	v.add(t, "nodes", node("n3", "w3", `{"type":"Ready","status":"False","reason":"KubeletNotReady"}`, `"spec":{"unschedulable":true},`))
	v.add(t, "nodes", node("n4", "w4", `{"type":"Ready","status":"True"},{"type":"DiskPressure","status":"True"}`, ""))
	// A Deployment through its ReplicaSet, two of three ready, one crashing.
	v.add(t, "deployments", `{"metadata":{"uid":"d1","name":"web","namespace":"shop","generation":2,"labels":{"app.kubernetes.io/name":"web","team":"x"}},
		"spec":{"replicas":3},"status":{"observedGeneration":2,"replicas":3,"readyReplicas":2,"updatedReplicas":3,
		"conditions":[{"type":"Available","status":"True","lastTransitionTime":"`+t0+`"}]}}`)
	v.add(t, "replicasets", `{"metadata":{"uid":"rs1","name":"web-7d9f8","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"web","controller":true}]},"spec":{"replicas":3}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p1","name":"web-7d9f8-a","namespace":"shop","creationTimestamp":"`+t0+`","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f8","controller":true}]},
		"spec":{"nodeName":"w1"},"status":{"phase":"Running","containerStatuses":[{"name":"web","ready":true}]}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p2","name":"web-7d9f8-b","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f8","controller":true}]},
		"spec":{"nodeName":"w1"},"status":{"phase":"Running","containerStatuses":[{"name":"web","ready":false,"restartCount":14,
		"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p3","name":"web-7d9f8-c","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f8","controller":true}]},
		"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/4 nodes are available: 4 Insufficient memory."}]}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p4","name":"bare","namespace":"batch","annotations":{"obs.cairnobs.org/alert":"off"}},
		"spec":{"nodeName":"w1"},"status":{"phase":"Pending","containerStatuses":[{"name":"x","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`)
	// Between back-offs: just exited, not waiting. Its Deployment warns
	// only, which its pods take before their namespace's page.
	v.add(t, "deployments", `{"metadata":{"uid":"d2","name":"api","namespace":"shop","annotations":{"obs.cairnobs.org/alert":"warn"}},"spec":{"replicas":1},"status":{"replicas":1,"updatedReplicas":1}}`)
	v.add(t, "replicasets", `{"metadata":{"uid":"rs2","name":"api-1","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"api","controller":true}]}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p6","name":"api-1-a","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"api-1","controller":true}]},
		"spec":{"nodeName":"w1"},"status":{"phase":"Running","containerStatuses":[{"name":"api","restartCount":4,"state":{"terminated":{"reason":"Error","exitCode":1}}}]}}`)
	v.add(t, "pods", `{"metadata":{"uid":"p5","name":"old","namespace":"shop","ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f8","controller":true}]},
		"status":{"phase":"Failed","reason":"Evicted"}}`)
	// A CronJob's runs: an old failure, then a newer success.
	v.add(t, "cronjobs", `{"metadata":{"uid":"c1","name":"backup","namespace":"batch"},"spec":{"schedule":"0 3 * * *"}}`)
	v.add(t, "jobs", `{"metadata":{"uid":"j1","name":"backup-1","namespace":"batch","creationTimestamp":"2026-11-12T03:00:00Z","ownerReferences":[{"kind":"CronJob","name":"backup","controller":true}]},
		"status":{"failed":3,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`)
	v.add(t, "jobs", `{"metadata":{"uid":"j2","name":"backup-2","namespace":"batch","creationTimestamp":"2026-11-13T03:00:00Z","ownerReferences":[{"kind":"CronJob","name":"backup","controller":true}]},
		"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`)
	v.add(t, "jobs", `{"metadata":{"uid":"j3","name":"migrate","namespace":"batch"},"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}}`)
	v.add(t, "persistentvolumeclaims", `{"metadata":{"uid":"v1","name":"data","namespace":"shop"},"spec":{"storageClassName":"pure-block"},"status":{"phase":"Pending"}}`)

	m := byName(build(v))
	want := map[string][2]string{
		"node:w1":                   {"ok", "Ready"},
		"node:w2":                   {"down", "Not responding (the kubelet stopped reporting)"},
		"node:w3":                   {"updating", "Cordoned, not ready"},
		"node:w4":                   {"degraded", "Disk pressure"},
		"controller:deployment/web": {"degraded", "2 of 3 ready"},
		"pod:web-7d9f8-a":           {"ok", "Running"},
		"pod:web-7d9f8-b":           {"crashloop", "Restarting (CrashLoopBackOff), last OOMKilled"},
		"pod:web-7d9f8-c":           {"pending", "Can't be scheduled: 0/4 nodes are available: 4 Insufficient memory."},
		"pod:bare":                  {"down", "Can't pull its image (ImagePullBackOff)"},
		"pod:old":                   {"stopped", "Evicted"},
		"controller:cronjob/backup": {"ok", "Scheduled (0 3 * * *)"},
		"job:backup-1":              {"stopped", "Failed (BackoffLimitExceeded); a later run has started"},
		"job:backup-2":              {"ok", "Succeeded"},
		"job:migrate":               {"down", "Failed (DeadlineExceeded)"},
		"pvc:data":                  {"pending", "Pending (waiting for a volume)"},
		"controller:deployment/api": {"down", "0 of 1 ready"},
		"pod:api-1-a":               {"crashloop", "Restarting (CrashLoopBackOff), last exit 1"},
	}
	for k, w := range want {
		r, ok := m[k]
		if !ok {
			t.Errorf("no record for %s", k)
			continue
		}
		if r.State != w[0] || r.Word != w[1] {
			t.Errorf("%s: %s %q, want %s %q", k, r.State, r.Word, w[0], w[1])
		}
	}
	if len(m) != len(want) {
		t.Errorf("%d records, want %d (the ReplicaSet a Deployment owns isn't one)", len(m), len(want))
	}
	web := m["pod:web-7d9f8-b"]
	if web.Owner != "deployment/web" || web.Node != "w1" || web.Alert != "page" || web.Facts["restarts"] != 14 || web.Facts["ready"] != "0/1" {
		t.Errorf("crashing pod: %+v", web)
	}
	if d := m["controller:deployment/web"]; d.Labels["app.kubernetes.io/name"] != "web" || len(d.Labels) != 1 || d.Since.Format(time.RFC3339) != t0 {
		t.Errorf("labels or since: %+v", d)
	}
	if m["pod:api-1-a"].Alert != "warn" || m["controller:deployment/api"].Alert != "warn" {
		t.Errorf("a pod takes its controller's alert setting: %+v", m["pod:api-1-a"])
	}
	if m["pod:bare"].Alert != "off" || m["job:migrate"].Alert != "" || m["job:backup-1"].Owner != "cronjob/backup" {
		t.Errorf("alerts and owners: %+v %+v", m["pod:bare"], m["job:backup-1"])
	}
	if n := m["node:w1"]; n.Facts["roles"] != "worker" || n.Facts["kubelet"] != "v1.32.2" || n.Namespace != "" {
		t.Errorf("node facts: %+v", n)
	}
	if c := m["pvc:data"]; c.Facts["storage_class"] != "pure-block" {
		t.Errorf("pvc facts: %+v", c)
	}
}

func TestControllerStates(t *testing.T) {
	cases := []struct {
		js, state, word string
	}{
		{`{"spec":{"replicas":0},"status":{}}`, "stopped", "Scaled to 0"},
		{`{"metadata":{"generation":3},"spec":{"replicas":3},"status":{"observedGeneration":3,"replicas":4,"readyReplicas":2,"updatedReplicas":1}}`, "updating", "Rolling out, 1 of 3 updated"},
		{`{"spec":{"replicas":2},"status":{"replicas":2,"readyReplicas":0,"updatedReplicas":2}}`, "down", "0 of 2 ready"},
		{`{"spec":{"replicas":2},"status":{"replicas":2,"readyReplicas":1,"updatedReplicas":1,"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded"}]}}`,
			"degraded", "Rollout stuck, 1 of 2 ready (ProgressDeadlineExceeded)"},
		{`{"status":{"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`, "ok", "1 of 1 ready"},
	}
	for _, c := range cases {
		js := strings.Replace(c.js, `{`, `{"metadata":{"uid":"d","name":"x"},`, 1)
		if strings.Contains(c.js, `"metadata"`) {
			js = strings.Replace(c.js, `"metadata":{`, `"metadata":{"uid":"d","name":"x",`, 1)
		}
		v := view{}
		v.add(t, "deployments", js)
		r := build(v)[0]
		if r.State != c.state || r.Word != c.word {
			t.Errorf("%s: %s %q, want %s %q", c.js, r.State, r.Word, c.state, c.word)
		}
	}
}

func TestClusterRecord(t *testing.T) {
	if r := clusterRecord("prod", "v1.32.2", nil, ""); r.State != "ok" || r.Facts["version"] != "v1.32.2" {
		t.Errorf("fine: %+v", r)
	}
	if r := clusterRecord("prod", "", map[string]string{"pods": "not allowed"}, ""); r.State != "degraded" || r.Word != "Can't read pods: not allowed" {
		t.Errorf("forbidden: %+v", r)
	}
	if r := clusterRecord("prod", "", nil, "dial tcp: i/o timeout"); r.State != "down" {
		t.Errorf("down: %+v", r)
	}
}
