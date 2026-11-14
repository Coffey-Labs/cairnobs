package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// clusterCopies is two copies with a host agent (host1) and a cluster
// agent (prod), and their tokens.
func clusterCopies(t *testing.T) (a, b *Server, hostTok, clusterTok string) {
	t.Helper()
	hostTok, clusterTok = NewAgentToken(), NewAgentToken()
	a, b = twoCopies(t, func(c *Config) {
		c.DevSignin = true
		c.Agents = []AgentConfig{{Name: "host1", TokenSHA256: HashToken(hostTok)}, {Name: "prod", TokenSHA256: HashToken(clusterTok), Cluster: true}}
	})
	return a, b, hostTok, clusterTok
}

func sendBatch(t *testing.T, srv *Server, tok string, b report.Workloads) {
	t.Helper()
	b.V = report.Version
	if b.Generation == "" {
		b.Generation = "g1"
	}
	body, _ := json.Marshal(b)
	if rec := post(srv.Handler(), "/api/v1/agent/workloads", tok, body, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body)
	}
}

// A small cluster: two nodes, a deployment with three pods (one crashing),
// a VM and a volume.
func smallCluster(now time.Time) []report.Workload {
	pod := func(i int, node, state, word string) report.Workload {
		return report.Workload{Kind: report.KindPod, ID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("web-%d", i), Namespace: "shop",
			Owner: "deployment/web", Node: node, State: state, Word: word, Since: now.Add(-time.Hour)}
	}
	return []report.Workload{
		{Kind: report.KindCluster, ID: "cluster", Name: "prod", State: report.StateOK, Word: "API answering", Facts: map[string]any{"version": "4.20.3"}},
		{Kind: report.KindNode, ID: "n1", Name: "worker-1", State: report.StateOK, Word: "Ready"},
		{Kind: report.KindNode, ID: "n2", Name: "worker-2", State: report.StateOK, Word: "Ready"},
		{Kind: report.KindController, ID: "d1", Name: "deployment/web", Namespace: "shop", State: report.StateDegraded, Word: "2 of 3 ready"},
		pod(1, "worker-1", report.StateOK, "Running"),
		pod(2, "worker-2", report.StateOK, "Running"),
		pod(3, "worker-2", report.StateCrashloop, "Restarting (CrashLoopBackOff)"),
		{Kind: report.KindVM, ID: "v1", Name: "db1", Namespace: "shop", Node: "worker-2", State: report.StateOK, Word: "Running", Alert: report.AlertPage},
		{Kind: report.KindPVC, ID: "c1", Name: "data", Namespace: "shop", State: report.StateOK, Word: "Bound"},
	}
}

func keysOf(c []Condition) string {
	var k []string
	for _, x := range c {
		k = append(k, x.Severity+":"+x.Key)
	}
	return strings.Join(k, " ")
}

// Pods are judged by their controller, unadopted workloads only warn, and a
// node that's down is one condition with what's on it.
func TestWorkloadsJudgedAndGrouped(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: smallCluster(now)})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	conds := a.workloadConditions(now)
	if got := keysOf(conds); got != "warning:workload/prod/shop/deployment/web" {
		t.Fatalf("conditions: %s", got)
	}
	if !strings.Contains(conds[0].Title, "1 of 3 pods: 1 restarting (CrashLoopBackOff)") || conds[0].Host != "prod" {
		t.Fatalf("title %q, host %q", conds[0].Title, conds[0].Host)
	}
	// worker-2 goes down: one condition for it, nothing for what's on it.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{{Kind: report.KindNode, ID: "n2", Name: "worker-2", State: report.StateDown, Word: "Not ready"}}})
	conds = a.workloadConditions(now)
	if got := keysOf(conds); got != "critical:node/prod/worker-2" || conds[0].Affected != "2 pods and 1 VM" {
		t.Fatalf("node down: %s (%q)", got, conds[0].Affected)
	}
	// It's back; the VM, which pages, is down.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{
		{Kind: report.KindNode, ID: "n2", Name: "worker-2", State: report.StateOK, Word: "Ready"},
		{Kind: report.KindVM, ID: "v1", Name: "db1", Namespace: "shop", Node: "worker-2", State: report.StateDown, Word: "ErrorUnschedulable", Alert: report.AlertPage},
	}})
	if got := keysOf(a.workloadConditions(now)); got != "critical:vm/prod/shop/db1 warning:workload/prod/shop/deployment/web" {
		t.Fatalf("vm down: %s", got)
	}
	// Silent: everything as last seen, one condition.
	if got := keysOf(a.workloadConditions(now.Add(3 * time.Minute))); got != "critical:cluster/prod/silent" {
		t.Fatalf("silent: %s", got)
	}
}

// A cluster agent sends no host report and isn't a host. A host agent may
// send workloads (a Proxmox node's, for its Proxmox cluster): they're a
// source under its own name.
func TestClusterAgentsAreNotHosts(t *testing.T) {
	a, _, hostTok, tok := clusterCopies(t)
	body, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g", Full: report.FullBegin,
		Records: []report.Workload{{Kind: report.KindVM, ID: "100", Name: "db", State: "ok", Word: "Running"}}})
	if rec := post(a.Handler(), "/api/v1/agent/workloads", hostTok, body, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("host agent sending workloads: %d %s", rec.Code, rec.Body)
	}
	var names []string
	for _, s := range a.workloadSnapshot() {
		names = append(names, s.name)
	}
	if strings.Join(names, " ") != "host1 prod" {
		t.Fatalf("sources %v", names)
	}
	rep, _ := json.Marshal(report.Report{V: report.Version})
	if rec := post(a.Handler(), "/api/v1/agent/report", tok, rep, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cluster agent sending a host report: %d", rec.Code)
	}
	// A kind from a newer agent is dropped; the rest of the batch counts.
	newer, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g", Records: []report.Workload{
		{Kind: "toaster", ID: "x", Name: "x", State: "ok"}, {Kind: report.KindNode, ID: "n", Name: "worker-1", State: "ok", Word: "Ready"}}})
	if rec := post(a.Handler(), "/api/v1/agent/workloads", tok, newer, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("an unknown kind: %d %s", rec.Code, rec.Body)
	}
	if s := a.workloadSnapshot(); len(s) != 2 || s[1].name != "prod" || len(s[1].recs) != 1 || s[1].recs[0].Kind != report.KindNode {
		t.Fatalf("kept %+v", s)
	}
	bad, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g", Records: []report.Workload{{Kind: report.KindNode, ID: "n", Name: "", State: "ok"}}})
	if rec := post(a.Handler(), "/api/v1/agent/workloads", tok, bad, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("a record with no name: %d", rec.Code)
	}
	hosts, _ := a.view(time.Now())
	if len(hosts) != 1 || hosts[0].Name != "host1" {
		t.Fatalf("hosts: %+v", hosts)
	}
}

// A batch taken by one copy reaches the other; so does a removal, and the
// end of a listing drops what it didn't include.
func TestWorkloadsReachTheOtherCopy(t *testing.T) {
	a, b, _, tok := clusterCopies(t) // on SQLite, b is the next start on a's file
	now := time.Now()
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: smallCluster(now)})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	b.syncWorkloads(false)
	count := func(s *Server) int {
		n := 0
		for _, sn := range s.workloadSnapshot() {
			n += len(sn.recs)
		}
		return n
	}
	if count(b) != 9 || keysOf(b.workloadConditions(now)) != "warning:workload/prod/shop/deployment/web" {
		t.Fatalf("b has %d: %s", count(b), keysOf(b.workloadConditions(now)))
	}
	sendBatch(t, a, tok, report.Workloads{Removed: []string{"p3"}})
	b.syncWorkloads(false)
	if count(b) != 8 {
		t.Fatalf("after a removal b has %d", count(b))
	}
	// A new listing with only the cluster and the nodes.
	sendBatch(t, a, tok, report.Workloads{Generation: "g2", Full: report.FullBegin, Records: smallCluster(now)[:3]})
	sendBatch(t, a, tok, report.Workloads{Generation: "g2", Full: report.FullEnd})
	b.syncWorkloads(false)
	if count(a) != 3 || count(b) != 3 {
		t.Fatalf("after the second listing: a %d, b %d", count(a), count(b))
	}
}

func TestWorkloadListFiltersAndPages(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	recs := smallCluster(now)
	for i := range 250 {
		recs = append(recs, report.Workload{Kind: report.KindPod, ID: fmt.Sprintf("x%03d", i), Name: fmt.Sprintf("batch-%03d", i), Namespace: "jobs",
			Owner: "job/nightly", Node: "worker-1", State: report.StateOK, Word: "Running"})
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: recs})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	h := a.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	get := func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), into)
	}
	type list struct {
		Workloads  []WorkloadRow  `json:"workloads"`
		Total      int            `json:"total"`
		Counts     map[string]int `json:"counts"`
		Kinds      map[string]int `json:"kinds"`
		Namespaces []string       `json:"namespaces"`
	}
	var l list
	get("/api/v1/workloads?limit=10", &l)
	if l.Total != 258 || len(l.Workloads) != 10 || l.Workloads[0].Name != "deployment/web" || l.Workloads[1].Name != "web-3" || l.Counts["problems"] != 2 || l.Kinds["pod"] != 253 {
		t.Fatalf("first page: total %d, first %+v, counts %v, kinds %v", l.Total, l.Workloads[0], l.Counts, l.Kinds)
	}
	l = list{}
	get("/api/v1/workloads?source=prod&show=problems", &l)
	if l.Total != 2 || len(l.Namespaces) != 2 {
		t.Fatalf("problems: %+v", l)
	}
	l = list{}
	get("/api/v1/workloads?source=prod&ns=jobs&q=batch-24&offset=5", &l)
	if l.Total != 10 || len(l.Workloads) != 5 {
		t.Fatalf("search, page 2: total %d, %d rows", l.Total, len(l.Workloads))
	}
	l = list{}
	get("/api/v1/workloads?node=worker-2", &l)
	if l.Total != 4 { // the node itself, two pods and the VM
		t.Fatalf("on worker-2: %d", l.Total)
	}
	var cl struct {
		Clusters []struct {
			ClusterView
			NodeList []map[string]any `json:"node_list"`
		} `json:"clusters"`
	}
	get("/api/v1/clusters", &cl)
	if len(cl.Clusters) != 1 || cl.Clusters[0].Version != "4.20.3" || cl.Clusters[0].NodesReady != 2 || cl.Clusters[0].Workloads != 256 ||
		cl.Clusters[0].Status != "warning" || len(cl.Clusters[0].NodeList) != 2 {
		t.Fatalf("clusters: %+v", cl.Clusters)
	}
}

// What has already fired stays as it is when its node goes down, or its
// cluster goes silent: nobody is told it's resolved when it isn't.
func TestFiredWorkloadsStayFired(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.tryLead(ctx, func(context.Context) {})
	a.alerts.cfg.For = 30 * time.Second // to fire before the cluster counts as silent
	now := time.Now()
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: smallCluster(now)})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	a.alerts.evaluate(now, a.workloadConditions(now))
	later := now.Add(a.alerts.cfg.For + time.Second)
	a.alerts.evaluate(later, a.workloadConditions(later))
	if !a.alerts.firing("workload/prod/shop/deployment/web") {
		t.Fatalf("the deployment's warning didn't fire: %s, for %s, open %+v", keysOf(a.workloadConditions(later)), a.alerts.cfg.For, a.alerts.open)
	}
	// worker-2, with the crashing pod on it, goes down.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{{Kind: report.KindNode, ID: "n2", Name: "worker-2", State: report.StateDown, Word: "Not ready"}}})
	conds := a.workloadConditions(later)
	if got := keysOf(conds); got != "critical:node/prod/worker-2 warning:workload/prod/shop/deployment/web" {
		t.Fatalf("node down: %s", got)
	}
	if !strings.Contains(conds[1].Title, "1 of 3 pods") {
		t.Fatalf("the deployment, as it was: %q", conds[1].Title)
	}
	// Silent: the one condition, and what had fired.
	if got := keysOf(a.workloadConditions(later.Add(3 * time.Minute))); got != "critical:cluster/prod/silent warning:workload/prod/shop/deployment/web" {
		t.Fatalf("silent: %s", got)
	}
}

// A cluster that joins through one copy is a cluster on the other too, not
// a host that has gone silent.
func TestJoinedClusterOnTheOtherCopy(t *testing.T) {
	a, b, _, _ := clusterCopies(t)
	now := time.Now()
	if err := a.db.AddJoinToken(state.JoinToken{TokenSHA256: HashToken("s3cret"), Agent: "kind", Cluster: true, Created: now, CreatedBy: "test", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.UseJoinToken(HashToken("s3cret"), now, a.inConfig); err != nil {
		t.Fatal(err)
	}
	b.reloadJoined()
	if !b.clusterAgent("kind") || !slices.Contains(b.clusterAgents(), "kind") {
		t.Fatalf("b doesn't know kind is a cluster agent: %v", b.clusterAgents())
	}
	hosts, conds := b.view(now.Add(10 * time.Minute))
	for _, h := range hosts {
		if h.Name == "kind" {
			t.Fatal("a joined cluster is listed as a host")
		}
	}
	for _, c := range conds {
		if c.Key == "silent/kind" {
			t.Fatal("a joined cluster went silent as a host")
		}
	}
}

// A Proxmox node's agent sends its cluster under its own name: it's on
// Clusters with the Proxmox cluster's name and version, its quorum lost is
// one critical condition, and its VMs are judged as any cluster's.
func TestProxmoxCluster(t *testing.T) {
	a, _, hostTok, _ := clusterCopies(t)
	self := func(state, word string) report.Workload {
		return report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "prod-pve", State: state, Word: word,
			Facts: map[string]any{"distribution": "Proxmox VE", "version": "8.4.1"}}
	}
	batch := func(full string, recs ...report.Workload) {
		body, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g1", Full: full, Records: recs})
		if rec := post(a.Handler(), "/api/v1/agent/workloads", hostTok, body, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("batch: %d %s", rec.Code, rec.Body)
		}
	}
	batch(report.FullBegin, self(report.StateOK, "Quorate"),
		report.Workload{Kind: report.KindNode, ID: "node/pve1", Name: "pve1", State: report.StateOK, Word: "Online"},
		report.Workload{Kind: report.KindVM, ID: "qemu/100", Name: "100 (db)", Node: "pve1", State: report.StateDown, Word: "HA can't run it (error)"})
	batch(report.FullEnd)
	now := time.Now()
	var pve *ClusterView
	for _, v := range a.clusterViews(now, nil) {
		if v.Name == "host1" {
			pve = &v
		}
	}
	if pve == nil || pve.Title != "prod-pve" || pve.Version != "Proxmox VE 8.4.1" || pve.Nodes != 1 || pve.Status != "good" {
		t.Fatalf("on Clusters: %+v", pve)
	}
	if c := find(a.workloadConditions(now), "vm/host1//100 (db)"); c == nil || c.Severity != "warning" {
		t.Fatalf("VM in HA error: %+v", c)
	}
	batch("", self(report.StateDown, "Not quorate: 1 of 3 nodes online"))
	conds := a.workloadConditions(now)
	if got := keysOf(conds); got != "critical:cluster/host1/silent" {
		t.Fatalf("not quorate: %s", got)
	}
	if conds[0].Title != "Cluster prod-pve (host1): not quorate: 1 of 3 nodes online" {
		t.Errorf("not quorate: %q", conds[0].Title)
	}
}

// One host agent reading several things: each its own source, its own
// card, forgotten when the agent stops reading it (docs/pure.md, 1).
func TestNamedSources(t *testing.T) {
	a, b, hostTok, _ := clusterCopies(t)
	self := func(name, dist string) report.Workload {
		return report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "API answering",
			Facts: map[string]any{"distribution": dist, "version": "1"}}
	}
	vm := report.Workload{Kind: report.KindVM, ID: "vm-1", Name: "db", State: report.StateOK, Word: "Running"}
	sendBatch(t, a, hostTok, report.Workloads{Source: "vcsa-01.example.org", Full: report.FullBegin, Records: []report.Workload{self("vcsa-01.example.org", "vCenter"), vm}})
	sendBatch(t, a, hostTok, report.Workloads{Source: "vcsa-01.example.org", Full: report.FullEnd})
	sendBatch(t, a, hostTok, report.Workloads{Source: "pc-01", Records: []report.Workload{self("pc-01", "Prism Central"), vm}})
	sendBatch(t, a, hostTok, report.Workloads{Records: []report.Workload{self("prod-pve", "Proxmox VE")}}) // an older agent's, unnamed
	body, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g1", Source: "Not a host name"})
	if rec := post(a.Handler(), "/api/v1/agent/workloads", hostTok, body, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad source name: %d", rec.Code)
	}
	cards := func(s *Server) map[string]ClusterView {
		s.syncWorkloads(false) // as the notice does
		m := map[string]ClusterView{}
		for _, v := range s.clusterViews(time.Now(), nil) {
			m[v.Name] = v
		}
		return m
	}
	for i, s := range []*Server{a, b} {
		m := cards(s)
		v, ok := m["host1:vcsa-01.example.org"]
		if !ok || v.Title != "vcsa-01.example.org" || v.ReadBy != "host1" || v.Workloads != 1 {
			t.Errorf("copy %d: the vCenter %+v", i, v)
		}
		if _, ok := m["host1:pc-01"]; !ok {
			t.Errorf("copy %d: no Prism Central: %v", i, m)
		}
		if v := m["host1"]; v.Title != "prod-pve" || v.ReadBy != "host1" {
			t.Errorf("copy %d: the unnamed one %+v", i, v)
		}
	}
	report1 := func(sources []string) {
		body, _ := json.Marshal(report.Report{V: report.Version, Host: "host1", Time: time.Now(), Sources: sources})
		if rec := post(a.Handler(), "/api/v1/agent/report", hostTok, body, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("report: %d %s", rec.Code, rec.Body)
		}
	}
	report1(nil) // an agent too old to say: nothing forgotten
	if m := cards(a); len(m) != 4 {
		t.Errorf("after an old report: %d cards", len(m))
	}
	report1([]string{"pc-01"})
	for i, s := range []*Server{a, b} {
		m := cards(s)
		_, pc := m["host1:pc-01"]
		_, vc := m["host1:vcsa-01.example.org"]
		_, bare := m["host1"]
		if !pc || vc || bare {
			t.Errorf("copy %d after reading only pc-01: %v", i, m)
		}
	}
	report1([]string{})
	if _, ok := cards(a)["host1:pc-01"]; ok {
		t.Error("pc-01 kept after the agent read nothing")
	}
}

// A FlashArray read by a host agent: said as an array, its controllers
// as controllers, its pods and connections as its parts (docs/pure.md).
func TestFlashArraySource(t *testing.T) {
	a, _, hostTok, _ := clusterCopies(t)
	sendBatch(t, a, hostTok, report.Workloads{Source: "fa-prod-01", Records: []report.Workload{
		{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "fa-prod-01", State: report.StateDegraded, Word: "Alert from Purity: Drive failure",
			Facts: map[string]any{"distribution": "Purity//FA", "version": "6.7.3"}},
		{Kind: report.KindNode, ID: "ct0", Name: "CT0", State: report.StateOK, Word: "Ready, primary"},
		{Kind: report.KindNode, ID: "ct1", Name: "CT1", State: report.StateDown, Word: "Not ready"},
		{Kind: report.KindCluster, ID: "pod/p-1", Name: "pod/prod-pod", State: report.StateUpdating, Word: "Resyncing on fa-prod-02 (43%)"},
		{Kind: report.KindCluster, ID: "connection/c-2", Name: "connection/fa-dr-01", State: report.StateDown, Word: "Not connected (trying)"},
		{Kind: report.KindPVC, ID: "v-1", Name: "k8s-pvc-0001", State: report.StateOK, Word: "Connected"},
	}})
	now := time.Now()
	var fa *ClusterView
	for _, v := range a.clusterViews(now, nil) {
		if v.Name == "host1:fa-prod-01" {
			fa = &v
		}
	}
	if fa == nil || fa.Version != "Purity//FA 6.7.3" || fa.Title != "fa-prod-01" || fa.ReadBy != "host1" || fa.Nodes != 2 || fa.NodesReady != 1 || len(fa.Parts) != 2 {
		t.Fatalf("on Clusters: %+v", fa)
	}
	titles := map[string]string{}
	for _, c := range a.workloadConditions(now) {
		titles[c.Key] = c.Title
	}
	for key, want := range map[string]string{
		"cluster/host1:fa-prod-01/cluster":             "Array fa-prod-01 (host1): alert from Purity: Drive failure",
		"node/host1:fa-prod-01/CT1":                    "Controller CT1 in fa-prod-01 (host1) is down",
		"cluster/host1:fa-prod-01/connection/fa-dr-01": "Replication to fa-dr-01 in fa-prod-01 (host1): not connected (trying)",
	} {
		if titles[key] != want {
			t.Errorf("%s: %q, want %q (all: %v)", key, titles[key], want, titles)
		}
	}
}

// A FlashBlade's blades are blades, and its file systems link to PVCs as
// a FlashArray's volumes do (docs/pure.md, step 5).
func TestFlashBladeSource(t *testing.T) {
	a, _, hostTok, clusterTok := clusterCopies(t)
	pv := "pvc-0f1e2d3c-4b5a-4687-9a0b-1c2d3e4f5a6b"
	sendBatch(t, a, hostTok, report.Workloads{Source: "fb-prod-01", Records: []report.Workload{
		{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "fb-prod-01", State: report.StateOK, Word: "API answering",
			Facts: map[string]any{"distribution": "Purity//FB", "version": "4.5.6"}},
		{Kind: report.KindNode, ID: "ch1.fb1", Name: "CH1.FB1", State: report.StateOK, Word: "Healthy"},
		{Kind: report.KindNode, ID: "ch1.fb7", Name: "CH1.FB7", State: report.StateDown, Word: "Critical: Blade is not responding"},
		{Kind: report.KindPVC, ID: "fs-1", Name: "px_8f3a2b1c-" + pv, State: report.StateOK, Word: "Shared over NFSv4.1", Facts: map[string]any{"type": "file system"}},
	}})
	sendBatch(t, a, clusterTok, report.Workloads{Records: []report.Workload{
		{Kind: report.KindCluster, ID: "cluster", Name: "prod", State: report.StateOK, Word: "API answering"},
		{Kind: report.KindPVC, ID: "u-1", Name: "shared", Namespace: "shop", State: report.StateOK, Word: "Bound", Facts: map[string]any{"volume": pv}},
	}})
	now := time.Now()
	var fb *ClusterView
	for _, v := range a.clusterViews(now, nil) {
		if v.Name == "host1:fb-prod-01" {
			fb = &v
		}
	}
	if fb == nil || fb.Version != "Purity//FB 4.5.6" || fb.Noun != "Array" || fb.NodeNoun != "Blade" || fb.Nodes != 2 || fb.NodesReady != 1 {
		t.Fatalf("on Clusters: %+v", fb)
	}
	conds := map[string]Condition{}
	for _, c := range a.workloadConditions(now) {
		conds[c.Key] = c
	}
	if c := conds["node/host1:fb-prod-01/CH1.FB7"]; c.Title != "Blade CH1.FB7 in fb-prod-01 (host1) is down" {
		t.Errorf("blade: %q", c.Title)
	}
	byPVC, _ := volumeLinks(a.clusterSnaps())
	if l := byPVC["prod/shop/shared"]; len(l) != 1 || l[0].Name != "px_8f3a2b1c-"+pv {
		t.Errorf("the PVC's file system: %+v", byPVC)
	}
}

// A PVC and the array volume behind it, linked by PersistentVolume name
// (docs/pure.md, step 3): each side shows the other, in rows and alerts.
func TestPVCVolumeLink(t *testing.T) {
	a, _, hostTok, clusterTok := clusterCopies(t)
	pv := "pvc-0f1e2d3c-4b5a-4687-9a0b-1c2d3e4f5a6b"
	sendBatch(t, a, clusterTok, report.Workloads{Records: []report.Workload{
		{Kind: report.KindCluster, ID: "cluster", Name: "prod", State: report.StateOK, Word: "API answering"},
		{Kind: report.KindPVC, ID: "u-1", Name: "data-db-0", Namespace: "shop", State: report.StateDown, Word: "Lost (its volume is gone)",
			Facts: map[string]any{"storage_class": "pure-block", "volume": pv}},
		{Kind: report.KindPVC, ID: "u-2", Name: "scratch", Namespace: "shop", State: report.StateOK, Word: "Bound",
			Facts: map[string]any{"volume": "pvc-99999999-4b5a-4687-9a0b-1c2d3e4f5a6b"}}, // on no array
	}})
	sendBatch(t, a, hostTok, report.Workloads{Source: "fa-prod-01", Records: []report.Workload{
		{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "fa-prod-01", State: report.StateDegraded, Word: "Hardware: CH0.BAY3 failed",
			Facts: map[string]any{"distribution": "Purity//FA", "version": "6.7.3"}},
		{Kind: report.KindPVC, ID: "v-1", Name: "prod-pod::k8s-" + pv, State: report.StateDegraded, Word: "Alert from Purity: Volume space exceeded"},
	}})
	now := time.Now()
	details := map[string]string{}
	for _, c := range a.workloadConditions(now) {
		details[c.Key] = c.Detail
	}
	for key, want := range map[string]string{
		"pvc/prod/shop/data-db-0":                   "Its array side: volume prod-pod::k8s-" + pv + " on fa-prod-01 (host1): alert from Purity: Volume space exceeded.",
		"pvc/host1:fa-prod-01//prod-pod::k8s-" + pv: "Used by PVC data-db-0 in shop (prod).",
		"cluster/host1:fa-prod-01/cluster":          "Its volumes hold 1 PVC, in prod.",
	} {
		if !strings.Contains(details[key], want) {
			t.Errorf("%s: %q, want it to hold %q (all: %v)", key, details[key], want, details)
		}
	}
	rec := get(a.Handler(), "/api/v1/workloads?kind=pvc", devCookie(t, a.Handler()))
	var got struct{ Workloads []WorkloadRow }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	links := 0
	for _, r := range got.Workloads {
		switch r.Name {
		case "data-db-0":
			if len(r.Volumes) != 1 || r.Volumes[0].Source != "host1:fa-prod-01" || r.Volumes[0].Array != "fa-prod-01 (host1)" {
				t.Errorf("the PVC's link %+v", r.Volumes)
			}
			links++
		case "prod-pod::k8s-" + pv:
			if len(r.PVCs) != 1 || r.PVCs[0].Source != "prod" || r.PVCs[0].Namespace != "shop" {
				t.Errorf("the volume's link %+v", r.PVCs)
			}
			links++
		case "scratch":
			if len(r.Volumes) != 0 {
				t.Errorf("scratch linked: %+v", r.Volumes)
			}
		}
	}
	if links != 2 {
		t.Errorf("%d linked rows of %d: %s", links, len(got.Workloads), rec.Body)
	}
}
