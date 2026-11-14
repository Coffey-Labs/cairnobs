package main

// Fake cluster agents (docs/cluster-watcher.md, How it will be tested):
// each one is a cluster of nodes, controllers with their pods, VMs with
// their volumes, sending a full listing at start and every -relist, and
// changes every 5 s: pods restarting, a rollout now and then, a node that
// goes down if asked.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

type clusterShape struct {
	nodes, controllers, pods, vms int
	churn                         float64       // pod changes a second
	relist                        time.Duration // between full listings
	nodeDown                      time.Duration // after start, take a node down (0: never)
	nodeDownFor                   time.Duration // for this long
	openshift                     bool          // cluster-01 is OpenShift, mid-update, with one operator degraded
}

type fakeCluster struct {
	name, token string
	shape       clusterShape
	mu          sync.Mutex
	gen         string
	recs        map[string]report.Workload
	pods        []string // ids, for picking
	changed     map[string]bool
	removed     []string
	seq         int
	noUsage     bool                               // its reader sends none (Nutanix: usage is a separate API, not read yet)
	source      string                             // what a host agent calls it (a vCenter's host name); "" for a cluster agent
	usageFn     func(now time.Time) []report.Usage // its own readings instead of CPU and memory (an array's performance)
}

func newID() string {
	return fmt.Sprintf("%08x-%04x-%012x", mrand.Uint32(), mrand.Uint32()&0xffff, mrand.Uint64()&0xffffffffffff)
}

func newCluster(name, token string, s clusterShape) *fakeCluster {
	c := &fakeCluster{name: name, token: token, shape: s, recs: map[string]report.Workload{}, changed: map[string]bool{}}
	now := time.Now()
	put := func(w report.Workload) { c.recs[w.ID] = w }
	// OpenShift's nodes run Red Hat Enterprise Linux CoreOS; a plain
	// Kubernetes cluster's, a general-purpose distribution.
	kubelet, nodeOS := "v1.34.2", "Ubuntu 24.04.3 LTS"
	if s.openshift && name == "cluster-01" {
		kubelet, nodeOS = "v1.33.5", "Red Hat Enterprise Linux CoreOS 9.6.20251023-0 (Plow)"
	}
	put(report.Workload{Kind: report.KindCluster, ID: "cluster", Name: name, State: report.StateOK, Word: "API answering", Facts: map[string]any{"version": kubelet}})
	for i := range s.nodes {
		put(report.Workload{Kind: report.KindNode, ID: newID(), Name: fmt.Sprintf("node-%02d", i), State: report.StateOK, Word: "Ready",
			Since: now.Add(-48 * time.Hour), Facts: map[string]any{"kubelet": kubelet, "os": nodeOS, "cpus": 32, "memory_gib": 256}})
	}
	k := clusterIndex(name)
	for i := range s.controllers {
		ns, owner := fmt.Sprintf("ns-%02d", i%30), fmt.Sprintf("deployment/app-%03d", i)
		want := s.pods / max(s.controllers, 1)
		put(report.Workload{Kind: report.KindController, ID: newID(), Name: owner, Namespace: ns, State: report.StateOK,
			Word: fmt.Sprintf("%d of %d ready", want, want), Facts: map[string]any{"desired": want, "ready": want},
			Labels: map[string]string{"app.kubernetes.io/part-of": appNames[i%30]}})
		put(svcRecord(fmt.Sprintf("app-%03d", i), ns, clusterIP(s.openshift && name == "cluster-01", k, i), "", "8080", []string{owner}, want, now))
		if i%50 == 7 { // a canary Service whose Deployment is scaled to nothing
			put(svcRecord(fmt.Sprintf("app-%03d-canary", i), ns, clusterIP(s.openshift && name == "cluster-01", k, 4000+i), "", "8080", nil, 0, now))
		}
		for range want {
			c.addPod(ns, owner, now)
		}
		if i%97 == 0 { // a few controllers with a crashing pod
			id := c.pods[len(c.pods)-1]
			p := c.recs[id]
			p.State, p.Word, p.Facts = report.StateCrashloop, "Restarting (CrashLoopBackOff)", map[string]any{"restarts": 40, "last_exit": "Error"}
			put(p)
		}
	}
	vmGroup := map[string][]string{}
	for i := range s.vms {
		node := fmt.Sprintf("node-%02d", mrand.N(max(s.nodes, 1)))
		g := vmGroups[i%len(vmGroups)]
		vmGroup[g.name] = append(vmGroup[g.name], fmt.Sprintf("vm/vm-%03d", i))
		// Its guest is what the fake host with its UUID says it runs.
		put(report.Workload{Kind: report.KindVM, ID: newID(), Name: fmt.Sprintf("vm-%03d", i), Namespace: "vms", Node: node, State: report.StateOK,
			Word: "Running", Since: now.Add(-24 * time.Hour), Facts: map[string]any{"guest_os": guestOS(distroFor(i * vmHostEvery).os),
				"firmware_uuid": vmUUID(name, i), "run_strategy": "Always", "agent_connected": true, "vcpus": 4, "memory_gib": 8},
			Labels: map[string]string{"app.kubernetes.io/part-of": g.name}})
		put(report.Workload{Kind: report.KindPVC, ID: newID(), Name: fmt.Sprintf("vm-%03d-disk", i), Namespace: "vms", State: report.StateOK,
			Word: "Bound", Facts: map[string]any{"storage_class": "pure-block", "capacity_gib": 50, "volume": pvName(name, i)}})
	}
	ocp := s.openshift && name == "cluster-01"
	for j, g := range vmGroups {
		if vms := vmGroup[g.name]; len(vms) > 0 {
			put(svcRecord(g.name, "vms", clusterIP(ocp, k, 3000+j), "", g.port, vms, len(vms), now))
		}
	}
	// The way in: one Ingress (a Route on OpenShift) per namespace, to its
	// first app; and a load balancer the fleet's hosts call.
	for n := range min(30, s.controllers) {
		app := appNames[n]
		kind, class := "ingress/", "nginx"
		if ocp {
			kind, class = "route/", "openshift-router"
		}
		host := fmt.Sprintf("%s.apps.%s.example.com", app, name)
		put(report.Workload{Kind: report.KindIngress, ID: newID(), Name: kind + app, Namespace: fmt.Sprintf("ns-%02d", n), State: report.StateOK,
			Word: fmt.Sprintf("%s → app-%03d", host, n), Since: now.Add(-30 * 24 * time.Hour),
			Facts: map[string]any{"hosts": host, "backends": fmt.Sprintf("app-%03d:8080", n), "tls": true, "class": class}})
	}
	clusterCerts(name, ocp, min(30, s.controllers), put)
	if s.controllers > 0 {
		put(svcRecord("gateway", "ns-00", clusterIP(ocp, k, 3999), lbAddr(k), "443", []string{"deployment/app-000"}, 3, now))
	}
	if ocp {
		part := func(id, n, state, word string, facts map[string]any) {
			put(report.Workload{Kind: report.KindCluster, ID: id, Name: n, State: state, Word: word, Since: now.Add(-20 * time.Minute), Facts: facts})
		}
		part("os-cv", "clusterversion/version", report.StateUpdating, "Updating to 4.20.4: Working towards 4.20.4: 512 of 873 done (58% complete)",
			map[string]any{"version": "4.20.3", "desired": "4.20.4", "distribution": "OpenShift"})
		part("os-mcp-master", "machineconfigpool/master", report.StateOK, "Up to date (3 nodes)", nil)
		part("os-mcp-worker", "machineconfigpool/worker", report.StateUpdating, fmt.Sprintf("Updating: 12 of %d nodes done", s.nodes), nil)
		for i, n := range []string{"authentication", "console", "dns", "etcd", "image-registry", "kube-apiserver", "machine-config", "network", "storage"} {
			part(fmt.Sprintf("os-co-%d", i), "clusteroperator/"+n, report.StateOK, "Available", nil)
		}
		part("os-co-ingress", "clusteroperator/ingress", report.StateDegraded, `Degraded: IngressDegraded: The "default" ingress controller reports Degraded=True`, nil)
		part("os-co-monitoring", "clusteroperator/monitoring", report.StateUpdating, "Progressing: Rolling out the stack.", nil)
	}
	if name == "cluster-00" { // the pod the probe changes
		put(report.Workload{Kind: report.KindPod, ID: "probe", Name: "probe", Namespace: "fleet", Node: "node-00", State: report.StateOK, Word: "Running"})
	}
	return c
}

// The fake clusters' apps, one per namespace, and the VM groups.
var appNames = []string{"storefront", "checkout", "catalog", "payments", "search", "accounts", "orders", "inventory", "shipping", "pricing",
	"reviews", "recommendations", "notifications", "billing", "ledger", "auth", "profile", "media", "reporting", "analytics",
	"ingest", "scheduler", "returns", "loyalty", "fraud", "tax", "quotes", "support", "warehouse", "partners"}

var vmGroups = []struct{ name, port string }{{"erp", "8443"}, {"warehouse-db", "5432"}, {"file-servers", "445"}, {"directory", "636"},
	{"build-agents", "22"}, {"legacy-crm", "8080"}, {"print", "631"}, {"backup-proxy", "8400"}, {"jump-hosts", "22"},
	{"message-queue", "5671"}, {"license-servers", "27000"}, {"reporting-db", "1521"}}

func clusterIndex(name string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(name, "cluster-"))
	return n
}

// clusterIP is Service i's address: OpenShift's service network is
// 172.30.0.0/16, kubeadm's 10.96.0.0/12.
func clusterIP(openshift bool, k, i int) string {
	if openshift {
		return fmt.Sprintf("172.30.%d.%d", i/250, 10+i%250)
	}
	return fmt.Sprintf("10.%d.%d.%d", 96+k%16, i/250, 10+i%250)
}

// lbAddr is cluster k's load balancer, which some fleet hosts call on :443.
func lbAddr(k int) string { return fmt.Sprintf("10.200.%d.10", k%250) }

func svcRecord(name, ns, ip, external, ports string, backends []string, ready int, now time.Time) report.Workload {
	w := report.Workload{Kind: report.KindService, ID: newID(), Name: name, Namespace: ns, State: report.StateOK,
		Since: now.Add(-30 * 24 * time.Hour), Facts: map[string]any{"type": "ClusterIP", "cluster_ips": ip, "ports": ports}}
	if external != "" {
		w.Facts["type"], w.Facts["external"] = "LoadBalancer", external
	}
	if len(backends) > 0 {
		w.Facts["backends"] = strings.Join(backends, ",")
	}
	switch ready {
	case 0:
		w.State, w.Word = report.StateStopped, "No ready endpoints"
	case 1:
		w.Word = "1 ready endpoint"
	default:
		w.Word = fmt.Sprintf("%d ready endpoints", ready)
	}
	return w
}

func (c *fakeCluster) addPod(ns, owner string, now time.Time) string {
	id := newID()
	c.recs[id] = report.Workload{Kind: report.KindPod, ID: id, Name: fmt.Sprintf("%s-%05x", strings.TrimPrefix(owner, "deployment/"), mrand.N(1<<20)),
		Namespace: ns, Owner: owner, Node: fmt.Sprintf("node-%02d", mrand.N(max(c.shape.nodes, 1))), State: report.StateOK, Word: "Running",
		Since: now.Add(-time.Duration(mrand.N(72)) * time.Hour), Labels: map[string]string{"app.kubernetes.io/name": strings.TrimPrefix(owner, "deployment/")},
		Facts: map[string]any{"restarts": 0, "ready": "1/1"}}
	c.pods = append(c.pods, id)
	return id
}

// churn changes n pods: most restart once, the odd one is replaced.
func (c *fakeCluster) churn(n int, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range n {
		if len(c.pods) == 0 {
			return
		}
		i := mrand.N(len(c.pods))
		id := c.pods[i]
		p := c.recs[id]
		if p.State == report.StateCrashloop {
			continue
		}
		if mrand.N(10) == 0 { // replaced: a new pod, the old one gone
			delete(c.recs, id)
			c.removed = append(c.removed, id)
			c.pods = slices.Delete(c.pods, i, i+1)
			nid := c.addPod(p.Namespace, p.Owner, now)
			q := c.recs[nid]
			q.State, q.Word, q.Since = report.StateStarting, "ContainerCreating", now
			c.recs[nid] = q
			c.changed[nid] = true
			continue
		}
		r, _ := p.Facts["restarts"].(int)
		p.Facts = map[string]any{"restarts": r + 1, "ready": "1/1", "last_exit": "OOMKilled"}
		p.State, p.Word, p.Since = report.StateOK, "Running", now
		c.recs[id] = p
		c.changed[id] = true
	}
	// Pods started last round are running now.
	for id, p := range c.recs {
		if p.Kind == report.KindPod && p.State == report.StateStarting && now.Sub(p.Since) > 4*time.Second {
			p.State, p.Word = report.StateOK, "Running"
			c.recs[id] = p
			c.changed[id] = true
		}
	}
}

func (c *fakeCluster) setNode(name, state, word string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, r := range c.recs {
		if r.Kind == report.KindNode && r.Name == name {
			r.State, r.Word, r.Since = state, word, now
			c.recs[id] = r
			c.changed[id] = true
		}
	}
}

// probe sets the probe pod's word, and returns it.
func (c *fakeCluster) probe(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	p := c.recs["probe"]
	p.Word, p.Since = fmt.Sprintf("Running (probe %d)", c.seq), now
	c.recs["probe"] = p
	c.changed["probe"] = true
	return p.Word
}

// batches is what to send now: a full listing in pages, or the changes.
func (c *fakeCluster) batches(full bool) []report.Workloads {
	out := c.listing(full)
	for i := range out {
		out[i].Source = c.source
	}
	return out
}

func (c *fakeCluster) listing(full bool) []report.Workloads {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !full {
		b := report.Workloads{V: report.Version, Agent: "fleet", Generation: c.gen, Records: []report.Workload{}, Removed: c.removed}
		for id := range c.changed {
			if r, ok := c.recs[id]; ok {
				b.Records = append(b.Records, r)
			}
		}
		c.changed, c.removed = map[string]bool{}, nil
		return []report.Workloads{b}
	}
	c.gen = newID()
	all := make([]report.Workload, 0, len(c.recs))
	for _, r := range c.recs {
		all = append(all, r)
	}
	c.changed, c.removed = map[string]bool{}, nil
	var out []report.Workloads
	for len(all) > 0 {
		n := min(len(all), 500)
		out = append(out, report.Workloads{V: report.Version, Agent: "fleet", Generation: c.gen, Full: report.FullPage, Records: all[:n]})
		all = all[n:]
	}
	if len(out) == 0 {
		out = append(out, report.Workloads{V: report.Version, Agent: "fleet", Generation: c.gen})
	}
	out[0].Full = report.FullBegin
	out = append(out, report.Workloads{V: report.Version, Agent: "fleet", Generation: c.gen, Full: report.FullEnd, Records: []report.Workload{}})
	return out
}

func (c *fakeCluster) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

// runCluster plays one cluster agent until ctx ends. A batch that fails is
// sent again before anything newer, as the agent does.
func runCluster(ctx context.Context, c *fakeCluster, client *http.Client, pick func() string, s *stats, probed func(word string, at time.Time)) {
	start := time.Now()
	var backlog []report.Workloads
	send := func() {
		for len(backlog) > 0 {
			body, _ := json.Marshal(backlog[0])
			t0 := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "POST", pick()+"/api/v1/agent/workloads", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				s.add(0, shortErr(err))
				return
			}
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				s.add(0, fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg))))
				return
			}
			s.add(time.Since(t0), "")
			backlog = backlog[1:]
		}
	}
	backlog = append(backlog, c.batches(true)...)
	send()
	lastList := time.Now()
	var lastUsage time.Time
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	down := false
	var probeWord string
	var probeAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if c.shape.nodeDown > 0 && c.name == "cluster-00" {
				el := now.Sub(start)
				if !down && el >= c.shape.nodeDown && el < c.shape.nodeDown+c.shape.nodeDownFor {
					down = true
					c.setNode("node-01", report.StateDown, "Not ready (kubelet stopped posting)", now)
					fmt.Printf("%s fleet: cluster-00 node-01 down\n", now.Format("15:04:05.000"))
				} else if down && el >= c.shape.nodeDown+c.shape.nodeDownFor {
					down = false
					c.shape.nodeDown = 0
					c.setNode("node-01", report.StateOK, "Ready", now)
					fmt.Printf("%s fleet: cluster-00 node-01 back\n", now.Format("15:04:05.000"))
				}
			}
			c.churn(int(c.shape.churn*5), now)
			if probed != nil {
				probeWord, probeAt = c.probe(now), now
			}
			if c.shape.relist > 0 && now.Sub(lastList) >= c.shape.relist {
				backlog = append(backlog, c.batches(true)...)
				lastList = now
			} else {
				backlog = append(backlog, c.batches(false)...)
			}
			// A usage reading a minute, as metrics.k8s.io gives the agent.
			if (!c.noUsage || c.usageFn != nil) && now.Sub(lastUsage) >= time.Minute {
				b := &backlog[len(backlog)-1]
				b.Usage, b.UsageAt = c.usage(now), now.Truncate(time.Second)
				if c.usageFn != nil {
					b.Usage = c.usageFn(now)
				}
				lastUsage = now
			}
			send()
			if probed != nil && len(backlog) == 0 {
				probed(probeWord, probeAt)
			}
		}
	}
}

// watchProbe times how long a probe change takes to show on each copy.
func watchProbe(ctx context.Context, client *http.Client, urls []string, cookies map[string]string, word string, at time.Time, s *stats) {
	for _, u := range urls {
		go func() {
			for ctx.Err() == nil && time.Since(at) < 30*time.Second {
				req, _ := http.NewRequestWithContext(ctx, "GET", u+"/api/v1/workloads?source=cluster-00&ns=fleet&q=probe", nil)
				req.Header.Set("Cookie", cookies[u])
				resp, err := client.Do(req)
				if err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if bytes.Contains(b, []byte(`"`+word+`"`)) {
						s.add(time.Since(at), "")
						return
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			if ctx.Err() == nil {
				s.add(0, "not shown within 30 s on "+u)
			}
		}()
	}
}

// signIn gets a dev sign-in cookie from each copy.
func signIn(ctx context.Context, client *http.Client, urls []string) (map[string]string, error) {
	cookies := map[string]string{}
	for _, u := range urls {
		req, _ := http.NewRequestWithContext(ctx, "POST", u+"/auth/dev", nil)
		req.Header.Set("X-Beacon", "1")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("dev sign-in on %s: %w (is dev_signin on?)", u, err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			cookies[u] = c.Name + "=" + c.Value
		}
		if cookies[u] == "" {
			return nil, fmt.Errorf("dev sign-in on %s: HTTP %d, no cookie (is dev_signin on?)", u, resp.StatusCode)
		}
	}
	return cookies, nil
}

// guestOS is what KubeVirt's guest agent says: the name without the
// release's code name, "Red Hat Enterprise Linux 9.6".
func guestOS(os string) string {
	name, _, _ := strings.Cut(os, " (")
	return name
}

// usage is a reading for every node, controller and VM: each settles
// around its own level (from its id), drifting over the day, with a few
// running near their memory limit so the pages have something to show.
func (c *fakeCluster) usage(now time.Time) []report.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	day := math.Sin(2 * math.Pi * float64(now.Hour()*60+now.Minute()) / 1440) // -1 … 1 over the day
	var out []report.Usage
	for _, r := range c.recs {
		h := fnv.New32a()
		h.Write([]byte(r.ID))
		seed := float64(h.Sum32()%1000) / 1000 // 0 … 1, fixed per record
		level := 0.2 + 0.5*seed + 0.1*day + 0.05*(mrand.Float64()-0.5)
		level = min(max(level, 0.02), 0.98)
		// Memory moves less over the day than CPU, and sits elsewhere.
		mem := min(max(0.3+0.45*float64(h.Sum32()/1000%1000)/1000+0.03*day, 0.05), 0.9)
		switch r.Kind {
		case report.KindNode:
			if r.State != report.StateOK {
				continue
			}
			cpus, gib := 32.0, 256.0 // a Kubernetes node's; Proxmox and ESXi nodes say their own
			if n, ok := r.Facts["cpus"].(int); ok {
				cpus = float64(n)
			}
			if g, ok := r.Facts["memory_gib"].(float64); ok {
				gib = g
			}
			out = append(out, report.Usage{ID: r.ID, CPU: math.Round(cpus*level*1000) / 1000, Mem: int64(gib * mem * (1 << 30)), CPUOf: cpus, MemOf: int64(gib * (1 << 30))})
		case report.KindController:
			pods, _ := r.Facts["desired"].(int)
			pods = max(pods, 1)
			u := report.Usage{ID: r.ID, CPU: math.Round(float64(pods)*0.5*level*1000) / 1000, Mem: int64(float64(pods) * mem * (1 << 30)), Pods: pods}
			if h.Sum32()%2 == 0 { // half have limits: half a core and 1 GiB a pod
				u.CPUOf, u.MemOf = float64(pods)*0.5, int64(pods)<<30
				if h.Sum32()%37 == 0 { // close to being OOM-killed
					u.Mem = int64(float64(u.MemOf) * (0.92 + 0.05*mrand.Float64()))
				}
			}
			out = append(out, u)
		case report.KindVM:
			if r.State == report.StateStopped {
				continue
			}
			cpus, gib := 4.0, 8.0 // a KubeVirt VM's; a Proxmox guest says its own
			if n, ok := r.Facts["vcpus"].(int); ok {
				cpus = float64(n)
			}
			if g, ok := r.Facts["memory_gib"].(float64); ok {
				gib = g
			}
			out = append(out, report.Usage{ID: r.ID, CPU: math.Round(cpus*level*1000) / 1000, Mem: int64(gib * mem * (1 << 30)), CPUOf: cpus, MemOf: int64(gib * (1 << 30)), Pods: 1})
		}
	}
	return out
}
