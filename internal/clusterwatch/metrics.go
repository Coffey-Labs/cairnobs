package clusterwatch

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
	"github.com/cairnobs/cairnobs/internal/report"
)

// CPU and memory (docs/cluster-watcher.md, step 5): metrics.k8s.io
// answers with what each node and each pod's containers use now. The agent
// reads it every metricsEvery and keeps the answers under "nodemetrics"
// and "podmetrics", beside the objects; the sender adds the sums to the
// next batch as Usage. Where nothing serves the API (no metrics-server),
// there's simply no usage: that's not a problem to report.

// metricsEvery is how often usage is read. A var for the tests.
var metricsEvery = time.Minute

const metricsRes = "metrics"

func watchMetrics(ctx context.Context, api API, s *store) {
	for ctx.Err() == nil && !s.allSynced() {
		sleep(ctx, time.Second)
	}
	for ctx.Err() == nil {
		readMetrics(ctx, api, s, time.Now())
		sleep(ctx, metricsEvery)
	}
}

// readMetrics reads every node's and pod's usage once.
func readMetrics(ctx context.Context, api API, s *store, now time.Time) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var nodes metricsList[nodeMetrics]
	err := api.Get(c, "/apis/metrics.k8s.io/v1beta1/nodes", &nodes)
	var pods metricsList[podMetrics]
	if err == nil {
		err = api.Get(c, "/apis/metrics.k8s.io/v1beta1/pods", &pods)
	}
	if ctx.Err() != nil {
		return
	}
	if kube.Code(err) == http.StatusNotFound {
		err = nil // not served here
	}
	s.setProblem(metricsRes, err)
	nm := map[string]any{}
	pm := map[string]any{}
	if err == nil {
		for i := range nodes.Items {
			nm[nodes.Items[i].Metadata.Name] = &nodes.Items[i]
		}
		for i := range pods.Items {
			pm[nsName(pods.Items[i].Metadata)] = &pods.Items[i]
		}
	}
	s.mu.Lock()
	s.objs["nodemetrics"], s.objs["podmetrics"] = nm, pm
	if len(nm) > 0 || len(pm) > 0 {
		s.usageAt = now.Truncate(time.Second)
	} else {
		s.usageAt = time.Time{}
	}
	s.mu.Unlock()
}

// usage sums what was read onto the records it belongs to: nodes, the
// controllers their pods belong to, and VMs through their launcher pods.
// recs are build's records for the same view.
func usage(v view, recs []report.Workload) []report.Usage {
	if len(v["nodemetrics"]) == 0 && len(v["podmetrics"]) == 0 {
		return nil
	}
	id := map[string]string{} // kind/namespace/name → record id
	for _, r := range recs {
		id[r.Kind+"/"+r.Namespace+"/"+r.Name] = r.ID
	}
	var out []report.Usage
	for _, n := range each[node](v, "nodes") {
		m, _ := v["nodemetrics"][n.Metadata.Name].(*nodeMetrics)
		rid := id[report.KindNode+"//"+n.Metadata.Name]
		if m == nil || rid == "" {
			continue
		}
		u := report.Usage{ID: rid, CPU: math.Round(cpuCores(m.Usage.CPU)*1000) / 1000, Mem: memBytes(m.Usage.Memory)}
		u.CPUOf, u.MemOf = cpuCores(n.Status.Allocatable["cpu"]), memBytes(n.Status.Allocatable["memory"])
		out = append(out, u)
	}
	b := newBuilder(v)
	sums := map[string]*report.Usage{}
	unlimited := map[string][2]bool{} // a pod with no CPU, no memory limit: no ceiling
	var order []string
	for _, p := range each[pod](v, "pods") {
		m, _ := v["podmetrics"][nsName(p.Metadata)].(*podMetrics)
		if m == nil {
			continue
		}
		owner, _ := b.podOwner(p.Metadata)
		key := report.KindController + "/" + p.Metadata.Namespace + "/" + owner
		if name, ok := strings.CutPrefix(owner, "virtualmachineinstance/"); ok {
			key = report.KindVM + "/" + p.Metadata.Namespace + "/" + name
		}
		rid := id[key]
		if owner == "" || rid == "" {
			continue // a bare pod, or a Job's: no history of its own
		}
		u := sums[rid]
		if u == nil {
			u = &report.Usage{ID: rid}
			sums[rid] = u
			order = append(order, rid)
		}
		u.Pods++
		for _, c := range m.Containers {
			u.CPU += cpuCores(c.Usage.CPU)
			u.Mem += memBytes(c.Usage.Memory)
		}
		nl := unlimited[rid]
		for _, c := range p.Spec.Containers {
			cpu, mem := c.Resources.Limits["cpu"], c.Resources.Limits["memory"]
			nl[0] = nl[0] || cpu == ""
			nl[1] = nl[1] || mem == ""
			u.CPUOf += cpuCores(cpu)
			u.MemOf += memBytes(mem)
		}
		unlimited[rid] = nl
	}
	for _, rid := range order {
		u := sums[rid]
		if nl := unlimited[rid]; nl[0] {
			u.CPUOf = 0
		}
		if nl := unlimited[rid]; nl[1] {
			u.MemOf = 0
		}
		u.CPU = math.Round(u.CPU*1000) / 1000 // millicores are as fine as it gets
		u.CPUOf = math.Round(u.CPUOf*1000) / 1000
		out = append(out, *u)
	}
	return out
}

func cpuCores(q string) float64 { f, _ := quantity(q); return f }

func memBytes(q string) int64 { f, _ := quantity(q); return int64(math.Round(f)) }

// quantity reads a Kubernetes quantity: "250m", "2", "123456n", "512Mi",
// "1.5Gi", "1e3", "128974848".
func quantity(q string) (float64, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, false
	}
	mult := 1.0
	for _, s := range suffixes {
		if strings.HasSuffix(q, s.s) {
			// "1e3" ends in no suffix; "2E" is exa. A suffix needs digits before it.
			if n := q[:len(q)-len(s.s)]; n != "" && strings.ContainsAny(n[len(n)-1:], "0123456789.") {
				q, mult = n, s.m
				break
			}
		}
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, false
	}
	return f * mult, true
}

// suffixes, longest first so "Mi" wins over "m"... and "M".
var suffixes = []struct {
	s string
	m float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
	{"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18},
}
