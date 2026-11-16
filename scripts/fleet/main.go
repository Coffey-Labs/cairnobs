// fleet is a synthetic fleet for the scale tests
// (docs/scale-and-availability.md, How it will be tested): many fake agents
// reporting to obs-beacond at the usual interval, with a browser's worth of
// interface requests on the side, timing both.
//
//	go run ./scripts/fleet config -n 1000 -clusters 3 -out dir    agents for obs-beacond.yaml, and their tokens
//	go run ./scripts/fleet run -server http://127.0.0.1:5404,http://127.0.0.1:5405 -tokens dir/tokens -for 5m
//
// Cluster agents (named cluster-NN) play a cluster each (cluster.go).
// Agents named win-* are Windows servers (windows.go).
//
// It's a test tool: tokens go over plain HTTP, and the interface requests
// sign in with dev_signin, so point it only at a loopback test server.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cairnobs/cairnobs/internal/netmap"
	"github.com/cairnobs/cairnobs/internal/report"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: fleet config|run [flags]")
	}
	switch os.Args[1] {
	case "config":
		config(os.Args[2:])
	case "run":
		run(os.Args[2:])
	default:
		log.Fatalf("fleet: unknown command %q", os.Args[1])
	}
}

func config(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	n := fs.Int("n", 1000, "agents")
	clusters := fs.Int("clusters", 0, "cluster agents")
	windows := fs.Int("windows", 0, "Windows servers, up to four (windows.go)")
	out := fs.String("out", ".", "where to write agents.yaml and tokens")
	demo := fs.Bool("demo", false, "also write the demo's checks.yaml, expected.yaml, backups.yaml, and demo.yaml with the lines for obs-beacond.yaml (demo.go)")
	_ = fs.Parse(args)
	var agents, tokens bytes.Buffer
	agents.WriteString("agents:\n")
	for i := range *n {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		tok := hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(tok))
		fmt.Fprintf(&agents, "  - { name: %s, token_sha256: %s }\n", hostName(i), hex.EncodeToString(sum[:]))
		fmt.Fprintf(&tokens, "%s %s\n", hostName(i), tok)
	}
	for i := range *clusters {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		tok := hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(tok))
		name := fmt.Sprintf("cluster-%02d", i)
		fmt.Fprintf(&agents, "  - { name: %s, token_sha256: %s, cluster: true }\n", name, hex.EncodeToString(sum[:]))
		fmt.Fprintf(&tokens, "%s %s\n", name, tok)
	}
	for _, c := range collectors {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		tok := hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(tok))
		fmt.Fprintf(&agents, "  - { name: %s, token_sha256: %s }\n", c.name, hex.EncodeToString(sum[:]))
		fmt.Fprintf(&tokens, "%s %s\n", c.name, tok)
	}
	for _, w := range winServers[:min(max(*windows, 0), len(winServers))] {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		tok := hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(tok))
		fmt.Fprintf(&agents, "  - { name: %s, token_sha256: %s }\n", w.name, hex.EncodeToString(sum[:]))
		fmt.Fprintf(&tokens, "%s %s\n", w.name, tok)
	}
	if err := os.WriteFile(filepath.Join(*out, "agents.yaml"), agents.Bytes(), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "tokens"), tokens.Bytes(), 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d agents, %d cluster agents, %d collectors and %d Windows servers: %s/agents.yaml goes into obs-beacond.yaml; %s/tokens is for fleet run\n",
		*n, *clusters, len(collectors), min(max(*windows, 0), len(winServers)), *out, *out)
	if *demo {
		writeDemo(*out, *n, *windows)
		fmt.Printf("the demo: %s/demo.yaml has the lines for obs-beacond.yaml; run fleet with -demo and the same -n\n", *out)
	}
}

type stats struct {
	mu   sync.Mutex
	lat  []time.Duration
	errs map[string]int
}

func (s *stats) add(d time.Duration, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != "" {
		s.errs[err]++
		return
	}
	s.lat = append(s.lat, d)
}

// take returns what came in since the last take, and starts again.
func (s *stats) take() ([]time.Duration, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.lat, s.errs
	s.lat, s.errs = nil, map[string]int{}
	return l, e
}

func newStats() *stats { return &stats{errs: map[string]int{}} }

func pct(l []time.Duration, p float64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	return l[min(len(l)-1, int(float64(len(l))*p))]
}

func summary(name string, l []time.Duration, e map[string]int, over time.Duration) string {
	slices.Sort(l)
	nerr := 0
	for _, n := range e {
		nerr += n
	}
	s := fmt.Sprintf("%-8s %6d ok (%6.1f/s)  p50 %7s  p95 %7s  p99 %7s  max %7s  errors %d",
		name, len(l), float64(len(l))/over.Seconds(), pct(l, .5).Round(100*time.Microsecond), pct(l, .95).Round(100*time.Microsecond),
		pct(l, .99).Round(100*time.Microsecond), pct(l, 1).Round(100*time.Microsecond), nerr)
	for k, n := range e {
		s += fmt.Sprintf("\n         %5d × %s", n, k)
	}
	return s
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	servers := fs.String("server", "http://127.0.0.1:8150", "obs-beacond's address; several, comma-separated, are taken in turn as a load balancer would")
	tokFile := fs.String("tokens", "tokens", "from fleet config")
	n := fs.Int("n", 0, "agents to run (0: every token)")
	interval := fs.Duration("interval", 15*time.Second, "between one agent's reports")
	containers := fs.Int("containers", 10, "containers per host (the cluster watcher's pods come later)")
	withMap := fs.Bool("map", true, "send map samples: sites of 50 hosts, WireGuard between sites, apps talking to databases")
	dur := fs.Duration("for", 5*time.Minute, "how long")
	every := fs.Duration("print", 30*time.Second, "how often to print")
	ui := fs.Duration("ui", 2*time.Second, "between interface requests (0: none); they sign in with dev_signin")
	var shape clusterShape
	fs.IntVar(&shape.nodes, "nodes", 30, "nodes per cluster")
	fs.IntVar(&shape.controllers, "controllers", 300, "controllers per cluster")
	fs.IntVar(&shape.pods, "pods", 3000, "pods per cluster")
	fs.IntVar(&shape.vms, "vms", 300, "VMs per cluster, each with a volume")
	fs.Float64Var(&shape.churn, "churn", 2, "pod changes a second, per cluster")
	fs.DurationVar(&shape.relist, "relist", 30*time.Minute, "between full listings")
	fs.DurationVar(&shape.nodeDown, "node-down", 0, "after this long, cluster-00's node-01 goes down (0: never)")
	fs.DurationVar(&shape.nodeDownFor, "node-down-for", time.Minute, "how long node-01 stays down")
	fs.BoolVar(&shape.openshift, "openshift", false, "make cluster-01 OpenShift, mid-update, with one operator degraded")
	pveGuests := fs.Int("proxmox", 0, "this many VMs and containers in a Proxmox cluster read by phx1-virt01's agent (0: none)")
	vsphereVMs := fs.Int("vsphere", 0, "this many VMs in a vCenter read by phx1-virt01's agent (0: none)")
	ntnxVMs := fs.Int("nutanix", 0, "this many VMs in a Prism Central read by phx1-virt01's agent (0: none)")
	pureN := fs.Int("pure", 0, "this many Pure arrays (up to 5: four FlashArrays with 600 volumes each, then a FlashBlade), all read by phx1-stor01's agent (0: none)")
	ibmiN := fs.Int("ibmi", 0, "this many IBM i partitions (up to 4), and the HMC of their Power servers, all read by phx1-ibmi01's agent (0: none)")
	akamaiDir := fs.String("akamai", "", "serve a fake Akamai account on loopback, and write its .edgerc and the akamai: section for obs-beacond.yaml to this directory's akamai.yaml (\"\": none)")
	chanDir := fs.String("channels", "", "catch alerts on loopback as ntfy, Discord, PagerDuty, a webhook and an email relay, and write their secrets and the alerts: and reports: sections for obs-beacond.yaml to this directory's channels.yaml (\"\": none)")
	mailDir := fs.String("mail", "", "serve a fake mail server on loopback, and write its password and the mail: section for obs-beacond.yaml to this directory's mail.yaml (\"\": none)")
	cfDir := fs.String("cloudflare", "", "serve a fake Cloudflare account on loopback, and write its token and the cloudflare: section for obs-beacond.yaml to this directory's cloudflare.yaml (\"\": none)")
	vaultDir := fs.String("vault", "", "serve two fake Vault clusters over HTTPS on loopback, and write their vault: list for obs-beacond.yaml to this directory's vault.yaml (\"\": none)")
	demo := fs.Bool("demo", false, "hosts report the demo's checks, units, backups and data layer, and fleet serves its Patroni cluster (fleet config -demo, same number of agents)")
	probe := fs.Bool("probe", true, "time how long a change in cluster-00 takes to show on every copy (needs dev_signin)")
	histDB := fs.String("history", "", "first write the last days into this obs-beacond database (SQLite; history.go), unless it has some already (\"\": none)")
	histKey := fs.String("state-key", "", "the database's state_key_file, for -history")
	histDays := fs.Int("history-days", 30, "how many days -history writes")
	_ = fs.Parse(args)

	type agent struct{ host, token string }
	var agents, clusterAgents, winAgents []agent
	collAgents := map[string]agent{}
	f, err := os.Open(*tokFile)
	if err != nil {
		log.Fatal(err)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		h, t, ok := strings.Cut(sc.Text(), " ")
		switch {
		case ok && strings.HasPrefix(h, "cluster-"):
			clusterAgents = append(clusterAgents, agent{h, t})
		case ok && isCollector(h):
			collAgents[h] = agent{h, t}
		case ok && strings.HasPrefix(h, winPrefix):
			if _, known := winServerFor(h); known {
				winAgents = append(winAgents, agent{h, t})
			}
		case ok:
			agents = append(agents, agent{h, t})
		}
	}
	f.Close()
	if *n > 0 && *n < len(agents) {
		agents = agents[:*n]
	}
	urls := strings.Split(*servers, ",")
	for _, u := range urls {
		if pu, err := url.Parse(u); err != nil || !isLoopback(pu.Hostname()) {
			log.Fatalf("fleet: %s: only a loopback test server", u)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	reports, pages, batches, shown := newStats(), newStats(), newStats(), newStats()
	// A load balancer: requests go only to copies whose /readyz passes,
	// checked every half second.
	ready := make([]atomic.Bool, len(urls))
	for i := range ready {
		ready[i].Store(true)
	}
	go func() {
		hc := &http.Client{Timeout: time.Second}
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			for i, u := range urls {
				resp, err := hc.Get(u + "/readyz")
				ok := err == nil && resp.StatusCode == 200
				if err == nil {
					resp.Body.Close()
				}
				if was := ready[i].Swap(ok); was != ok {
					log.Printf("%s lb: %s %s", time.Now().Format("15:04:05.000"), u, map[bool]string{true: "ready", false: "out"}[ok])
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	var turn atomic.Uint64
	pick := func() string {
		for range len(urls) {
			i := turn.Add(1) % uint64(len(urls))
			if ready[i].Load() {
				return urls[i]
			}
		}
		return urls[turn.Load()%uint64(len(urls))] // none ready: try anyway, and count the failure
	}

	fleetSites = max(1, (len(agents)+siteSize-1)/siteSize)
	fleetClusters = len(clusterAgents)
	var demoAll []demoCheck
	if *demo {
		demoAll = demoChecks(len(agents))
		if hasDataLayer(len(agents)) {
			servePatroni(ctx)
		}
	}
	var made, delivered, queued atomic.Int64
	log.Printf("fleet: %d agents every %s, %d containers each, to %s, for %s", len(agents), *interval, *containers, *servers, *dur)
	all := append(slices.Clone(agents), winAgents...)
	for _, c := range collectors {
		if a, ok := collAgents[c.name]; ok {
			all = append(all, a)
		}
	}
	hosts := make([]*host, len(all))
	for i, a := range all {
		var h *host
		c, isColl := collectorFor(a.host)
		switch {
		case isColl: // collectors.go
			h = newCollectorHost(c, *interval)
		case i >= len(agents): // a Windows server (windows.go)
			h = newWindowsHost(a.host, *interval)
		default:
			h = newHost(i, a.host, *containers, *interval)
		}
		if *demo && h.win == nil && !isColl {
			h.demo = newDemoHost(fleetIndex(i, a.host), len(agents), demoAll)
			h.demo.setup(&h.r)
		}
		if *withMap && h.win == nil && !isColl {
			h.m = fakeMap(i, a.host, h.r.Containers)
			h.certs, h.files = hostCerts(i, a.host)
		}
		hosts[i] = h
	}
	// The clusters, made now so the history can be written from them.
	type sim struct {
		c     *fakeCluster
		probe bool
	}
	var sims []sim
	for _, a := range clusterAgents {
		sims = append(sims, sim{newCluster(a.host, a.token, shape), a.host == "cluster-00"})
	}
	virt, virtOK := collAgents[virtCollector.name]
	if *pveGuests > 0 && virtOK {
		a := virt
		sims = append(sims, sim{c: newProxmox(a.host, a.token, *pveGuests)})
		log.Printf("fleet: Proxmox cluster %s, %d nodes, %d guests, read by %s", proxmoxCluster, proxmoxNodes, *pveGuests, a.host)
	}
	if *vsphereVMs > 0 && virtOK {
		a := virt
		sims = append(sims, sim{c: newVSphere(a.host, a.token, *vsphereVMs)})
		log.Printf("fleet: vCenter %s, %d ESXi hosts, %d VMs, read by %s", vcenterName, esxiHosts, *vsphereVMs, a.host)
	}
	if *ntnxVMs > 0 && virtOK {
		a := virt
		sims = append(sims, sim{c: newNutanix(a.host, a.token, *ntnxVMs)})
		log.Printf("fleet: Prism Central %s, %d clusters of %d AHV hosts, %d VMs, read by %s", prismCentral, ntnxClusters, ahvPerPE, *ntnxVMs, a.host)
	}
	if a, ok := collAgents[storageCollector.name]; ok && *pureN > 0 {
		n := min(*pureN, len(pureArrays))
		for k := range n {
			sims = append(sims, sim{c: newPure(a.host, a.token, k)})
		}
		log.Printf("fleet: %d Pure arrays (%v), read by %s", n, pureArrays[:n], a.host)
	}
	if a, ok := collAgents[ibmiCollector.name]; ok && *ibmiN > 0 {
		n := min(*ibmiN, len(ibmiPartitions))
		for k := range n {
			sims = append(sims, sim{c: newIBMi(a.host, a.token, k)})
		}
		sims = append(sims, sim{c: newHMC(a.host, a.token)})
		log.Printf("fleet: %d IBM i partitions (%v) and the HMC %s, read by %s", n, ibmiPartitions[:n], hmcName, a.host)
	}
	if *histDB != "" {
		past := pastFleet{hosts: hosts, vaults: *vaultDir != "", mail: *mailDir != ""}
		for _, s := range sims {
			past.clusters = append(past.clusters, s.c)
		}
		for _, c := range demoAll {
			past.checks = append(past.checks, c.id)
		}
		if err := writeHistory(*histDB, *histKey, *histDays, past, time.Now()); err != nil {
			log.Fatalf("fleet: history: %v", err)
		}
	}
	var wg sync.WaitGroup
	for i, a := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := hosts[i]
			// Agents start at random points in the interval, as a fleet does.
			select {
			case <-ctx.Done():
				return
			case <-time.After(mrand.N(*interval)):
			}
			t := time.NewTicker(*interval)
			defer t.Stop()
			// Like the agent, keep what couldn't be sent and send it again,
			// oldest first, all but the newest marked as queued.
			var backlog [][]byte
			defer func() { queued.Add(int64(len(backlog))) }()
			send := func(body []byte, old bool) bool {
				start := time.Now()
				req, _ := http.NewRequestWithContext(ctx, "POST", pick()+"/api/v1/agent/report", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+a.token)
				req.Header.Set("Content-Type", "application/json")
				if old {
					req.Header.Set("X-Beacon-Queued", "1")
				}
				resp, err := client.Do(req)
				switch {
				case ctx.Err() != nil:
					return false
				case err != nil:
					reports.add(0, shortErr(err))
					return false
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode/100 != 2 {
					reports.add(0, fmt.Sprintf("HTTP %d", resp.StatusCode))
					return false
				}
				reports.add(time.Since(start), "")
				delivered.Add(1)
				return true
			}
			for {
				body, _ := json.Marshal(h.next(time.Now()))
				made.Add(1)
				backlog = append(backlog, body)
				for len(backlog) > 0 && send(backlog[0], len(backlog) > 1) {
					backlog = backlog[1:]
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	if len(clusterAgents) > 0 {
		log.Printf("fleet: %d clusters, each %d nodes, %d controllers, %d pods, %d VMs; %.1f pod changes/s each", len(clusterAgents),
			shape.nodes, shape.controllers, shape.pods, shape.vms, shape.churn)
	}
	var cookies map[string]string
	if *probe && len(clusterAgents) > 0 {
		if cookies, err = signIn(ctx, client, urls); err != nil {
			log.Fatalf("fleet: %v", err)
		}
	}
	for _, s := range sims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var probed func(string, time.Time)
			if cookies != nil && s.probe {
				probed = func(word string, at time.Time) { watchProbe(ctx, client, urls, cookies, word, at, shown) }
			}
			runCluster(ctx, s.c, client, pick, batches, probed)
		}()
	}
	if *vaultDir != "" {
		serveVaults(ctx, *vaultDir)
	}
	if *cfDir != "" {
		serveCloudflare(ctx, *cfDir)
	}
	if *mailDir != "" {
		serveMail(ctx, *mailDir)
	}
	if *chanDir != "" {
		serveChannels(ctx, *chanDir)
	}
	if *akamaiDir != "" {
		serveAkamai(ctx, *akamaiDir)
	}
	if *ui > 0 && len(agents) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			extra := []string{}
			if len(clusterAgents) > 0 {
				extra = []string{"/api/v1/clusters", "/api/v1/workloads?show=problems", "/api/v1/workloads?source=cluster-01&ns=ns-07&offset=100"}
			}
			browse(ctx, client, urls, pick, agents[0].host, *ui, pages, extra)
		}()
	}
	t := time.NewTicker(*every)
	defer t.Stop()
	last := time.Now()
	var allR, allP, allB, allS []time.Duration
	errR, errP, errB, errS := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	print := func() {
		now := time.Now()
		l, e := reports.take()
		pl, pe := pages.take()
		line := fmt.Sprintf("%s\n%s\n%s", now.Format("15:04:05"), summary("reports", l, e, now.Sub(last)), summary("pages", pl, pe, now.Sub(last)))
		if len(clusterAgents) > 0 {
			bl, be := batches.take()
			sl, se := shown.take()
			line += "\n" + summary("batches", bl, be, now.Sub(last)) + "\n" + summary("shown", sl, se, now.Sub(last))
			allB, allS = append(allB, bl...), append(allS, sl...)
			for k, v := range be {
				errB[k] += v
			}
			for k, v := range se {
				errS[k] += v
			}
		}
		log.Print(line)
		allR, allP = append(allR, l...), append(allP, pl...)
		for k, v := range e {
			errR[k] += v
		}
		for k, v := range pe {
			errP[k] += v
		}
		last = now
	}
	start := time.Now()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			print()
		}
	}
	wg.Wait()
	print()
	log.Printf("all %s\n%s\n%s", time.Since(start).Round(time.Second), summary("reports", allR, errR, time.Since(start)), summary("pages", allP, errP, time.Since(start)))
	if len(clusterAgents) > 0 {
		log.Printf("%s\n%s", summary("batches", allB, errB, time.Since(start)), summary("shown", allS, errS, time.Since(start)))
	}
	// Every report made is either delivered or still queued at the end;
	// none dropped. An agent sends its queue once obs-beacond is back.
	log.Printf("made %d reports: %d delivered, %d still queued at the end, %d lost",
		made.Load(), delivered.Load(), queued.Load(), made.Load()-delivered.Load()-queued.Load())
}

func isLoopback(h string) bool {
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// browse is someone looking at the interface: the pages' API calls in
// turn, through the load balancer, signed in once on each copy (sessions
// are shared, but each copy's own cookie keeps this independent of that).
func browse(ctx context.Context, client *http.Client, urls []string, pick func() string, host string, every time.Duration, s *stats, extra []string) {
	cookies, err := signIn(ctx, client, urls)
	if err != nil {
		log.Printf("fleet: %v", err)
		return
	}
	paths := append([]string{"/api/v1/overview", "/api/v1/containers", "/api/v1/incidents", "/api/v1/hosts/" + host, "/api/v1/checks", "/api/v1/reports"}, extra...)
	t := time.NewTicker(every)
	defer t.Stop()
	for i := 0; ; i++ {
		u := pick()
		req, _ := http.NewRequestWithContext(ctx, "GET", u+paths[i%len(paths)], nil)
		req.Header.Set("Cookie", cookies[u])
		start := time.Now()
		resp, err := client.Do(req)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.add(0, shortErr(err))
		default:
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				s.add(0, fmt.Sprintf("%s HTTP %d", paths[i%len(paths)], resp.StatusCode))
			} else {
				s.add(time.Since(start), "")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// host makes one fake host's reports: steady numbers with some noise, a
// few disks, and containers that mostly run.
type host struct {
	r     report.Report
	start time.Time

	m       *report.Map // its map sample, sent once a minute as the agent does
	mapSent time.Time

	vms []report.Workload // its libvirt VMs, when it's a hypervisor

	demo *demoHost // what it reports for the demo (demo.go), when it runs
	win  *winHost  // what a Windows server reports (windows.go)

	certs     *report.CertScan  // what its ports serve (certs.go)
	files     *report.CertFiles // what its files hold
	certsSent time.Time
}

// hypervisorEvery says which fake hosts run libvirt: host 1 in every 20
// that is an app server (isHypervisor),
// with libvirtVMs VMs. Hosts 2, 6, 10, 14 and 18 in the same 20 are its
// first five VMs, with their domain UUIDs as SMBIOS UUIDs.
const (
	hypervisorEvery = 20
	libvirtVMs      = 12
)

var vmRoles = [libvirtVMs]string{"db", "web", "cache", "build", "mq", "ldap", "backup", "monitor", "runner", "jump", "proxy", "spare"}

// hypervisorVMs are host i's domains: all running but its spare, which is
// shut off.
func hypervisorVMs(i int, now time.Time) []report.Workload {
	out := make([]report.Workload, libvirtVMs)
	for k := range out {
		id := vmUUID("libvirt-"+hostName(i), k)
		r := report.Workload{Kind: report.KindVM, ID: id, Name: fmt.Sprintf("%s-%02d", vmRoles[k], i/hypervisorEvery),
			State: report.StateOK, Word: "Running", Since: now.Add(-time.Duration(3+k) * 24 * time.Hour).Truncate(time.Second),
			Facts: map[string]any{"firmware_uuid": id, "autostart": k != libvirtVMs-1, "hypervisor": "libvirt", "vcpus": 2 << (k % 3), "memory_gib": float64(int(4) << (k % 3))}}
		if k == libvirtVMs-1 {
			r.State, r.Word = report.StateStopped, "Shut off"
		}
		out[k] = r
	}
	return out
}

// vmUsage is what the running VMs use now: a steady share each, with noise.
func vmUsage(vms []report.Workload) []report.Usage {
	var out []report.Usage
	for k, r := range vms {
		if r.State != report.StateOK {
			continue
		}
		cpus, _ := r.Facts["vcpus"].(int)
		gib, _ := r.Facts["memory_gib"].(float64)
		share := 0.15 + 0.06*float64(k) + 0.1*mrand.Float64()
		out = append(out, report.Usage{ID: r.ID, CPU: math.Round(float64(cpus)*share*1000) / 1000, CPUOf: float64(cpus),
			Mem: int64(gib * (0.4 + 0.04*float64(k)) * (1 << 30)), MemOf: int64(gib * (1 << 30))})
	}
	return out
}

// distro is one operating system in the fleet's mix.
type distro struct {
	os, kernel      string
	rootDev, rootFS string
	data            string // where containers live
}

var (
	rhel9  = distro{"Red Hat Enterprise Linux 9.6 (Plow)", "5.14.0-570.52.1.el9_6.x86_64", "/dev/mapper/rhel-root", "xfs", "/var/lib/containers"}
	rhel8  = distro{"Red Hat Enterprise Linux 8.10 (Ootpa)", "4.18.0-553.81.1.el8_10.x86_64", "/dev/mapper/rhel-root", "xfs", "/var/lib/containers"}
	rhel10 = distro{"Red Hat Enterprise Linux 10.0 (Coughlan)", "6.12.0-55.39.1.el10_0.x86_64", "/dev/mapper/rhel-root", "xfs", "/var/lib/containers"}
	ubuntu = distro{"Ubuntu 24.04.3 LTS", "6.8.0-87-generic", "/dev/nvme0n1p2", "ext4", "/var/lib/docker"}
	debian = distro{"Debian GNU/Linux 13 (trixie)", "6.12.48+deb13-amd64", "/dev/nvme0n1p2", "ext4", "/var/lib/docker"}
)

// distroFor is a Red Hat shop's mix, in every 20 hosts: 12 on RHEL 9,
// 3 still on RHEL 8, 2 already on RHEL 10, 2 Ubuntu and 1 Debian.
func distroFor(i int) distro {
	switch n := i % 20; {
	case n == 7 || n == 13 || n == 18:
		return rhel8
	case n == 4 || n == 15:
		return rhel10
	case n == 9 || n == 17:
		return ubuntu
	case n == 11:
		return debian
	}
	return rhel9
}

// vmHostEvery says which fake hosts are VMs: every fourth is cluster-00's
// VM number i/4, with that VM's firmware UUID as its SMBIOS UUID, so the
// server links them (docs/cluster-watcher.md, Linking a VM to its agent).
const vmHostEvery = 4

// vmUUID is a fake VM's firmware UUID: the same for a cluster and number
// on every run.
func vmUUID(cluster string, i int) string {
	h := fnv.New32a()
	h.Write([]byte(cluster))
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", h.Sum32(), i)
}

// pvName is a fake PVC's PersistentVolume, as Kubernetes names it: what
// the array volume behind it is named after (docs/pure.md, step 3).
func pvName(cluster string, i int) string { return "pvc-" + vmUUID("pv-"+cluster, i) }

func newHost(i int, name string, containers int, interval time.Duration) *host {
	h := &host{start: time.Now().Add(-time.Duration(mrand.N(30*24)) * time.Hour)}
	d := distroFor(i)
	h.r = report.Report{
		V: report.Version, Agent: "fleet", Host: name, Interval: int(interval / time.Second),
		Kernel: d.kernel, OS: d.os, CPUs: 4 << (i % 3),
		Mem: report.Mem{Total: 16 << 30, SwapTotal: 4 << 30, SwapFree: 4 << 30},
		Disks: []report.Disk{
			{Mount: "/", Device: d.rootDev, FS: d.rootFS, Size: 100 << 30},
			{Mount: d.data, Device: "/dev/nvme1n1", FS: "xfs", Size: 500 << 30},
		},
		Sources: []string{}, // what it reads beside itself, as the agent says
	}
	list := make([]report.Container, containers)
	for c := range list {
		list[c] = report.Container{ID: fmt.Sprintf("%012x", mrand.Uint64()), Name: fmt.Sprintf("app-%02d", c), Image: fmt.Sprintf("registry.example.org/app-%02d:1.%d", c, c%7),
			Project: "stack", State: "running", Status: "Up 3 days (healthy)", Health: "healthy", Created: h.start.Unix()}
	}
	h.r.Containers = &report.Containers{List: list}
	switch {
	case i%vmHostEvery == 0:
		h.r.Machine = vmUUID("cluster-00", i/vmHostEvery)
	case isHypervisor(i):
		h.vms = hypervisorVMs(i, time.Now())
		h.r.CPUs, h.r.Mem.Total = 64, 512<<30
	case i%vmHostEvery == 2 && isHypervisor(i/hypervisorEvery*hypervisorEvery+1):
		h.r.Machine = vmUUID("libvirt-"+hostName(i/hypervisorEvery*hypervisorEvery+1), (i%hypervisorEvery-2)/vmHostEvery)
	case i%proxmoxGuestEvery == 3:
		h.r.Machine = vmUUID("proxmox-"+proxmoxCluster, i/proxmoxGuestEvery)
	case i%vsphereGuestEvery == 7:
		h.r.Machine = vmUUID("vsphere-"+vcenterName, i/vsphereGuestEvery)
		if (i/vsphereGuestEvery)%2 == 1 {
			h.r.Machine = report.SwappedUUID(h.r.Machine) // older virtual hardware
		}
	case i%nutanixGuestEvery == 11:
		h.r.Machine = vmUUID("nutanix-"+prismCentral, i/nutanixGuestEvery)
	}
	return h
}

func (h *host) next(now time.Time) report.Report {
	r := h.r
	r.Time = now.UTC()
	r.Uptime = now.Sub(h.start).Seconds()
	r.CPU = math.Round(cpuNow(r.Host, now)*10) / 10 // load.go
	cores := float64(max(r.CPUs, 1))
	r.Load = [3]float64{cores * r.CPU / 100, cores * cpuAt(r.Host, now.Add(-5*time.Minute)) / 100, cores * cpuAt(r.Host, now.Add(-15*time.Minute)) / 100}
	r.Mem.Available = uint64(float64(r.Mem.Total) * (1 - memAt(r.Host, now)/100))
	busy := 0.2 + workday(r.Host, now)
	r.Net = report.Net{RxBps: 4e5 * busy * (1 + 0.3*jitter(r.Host, now)), TxBps: 1.5e5 * busy * (1 + 0.3*jitter(r.Host+" tx", now))}
	disks := slices.Clone(r.Disks)
	for i := range disks {
		disks[i].Used = uint64(float64(disks[i].Size) * diskAt(r.Host, i, now))
		disks[i].Avail = disks[i].Size - disks[i].Used
	}
	r.Disks = disks
	if h.vms != nil {
		r.VMs = &report.VMs{List: h.vms, Usage: vmUsage(h.vms)}
	}
	h.stampCerts(&r, now)
	if h.demo != nil {
		h.demo.stamp(&r, h.start, now)
	}
	if h.win != nil {
		h.win.stamp(h, &r, now)
	}
	if h.m != nil && now.Sub(h.mapSent) >= netmap.SendEvery-time.Duration(r.Interval)*time.Second/2 {
		// Handshakes are a minute or so old each time, as a live link's are.
		for d := range h.m.WireGuard {
			for p := range h.m.WireGuard[d].Peers {
				h.m.WireGuard[d].Peers[p].Handshake = now.Add(-time.Duration(30+p%60) * time.Second).UTC()
			}
		}
		r.Map, h.mapSent = h.m, now
	}
	return r
}
