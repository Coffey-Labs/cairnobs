// obs-beacon-agent is the one agent Cairn OBS runs on each host. It reads the host
// and its containers and pushes a report to obs-beacond every interval.
//
//	obs-beacon-agent -config /etc/obs-beacon/agent.yaml   collect and report (unprivileged)
//	obs-beacon-agent helper -listen ... -docker ...    the read-only root helper
//	obs-beacon-agent scan -config ... -host ... -out ... one backup scan (root, on a timer)
//	obs-beacon-agent join [-force] <token>              join a server with a token from Hosts, Add host (root)
//	obs-beacon-agent cluster                            the cluster agent, in a cluster (deploy/kubernetes/cluster-agent)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/agentlink"
	"github.com/cairnobs/cairnobs/internal/certfiles"
	"github.com/cairnobs/cairnobs/internal/certreload"
	"github.com/cairnobs/cairnobs/internal/check"
	"github.com/cairnobs/cairnobs/internal/collect"
	"github.com/cairnobs/cairnobs/internal/docker"
	"github.com/cairnobs/cairnobs/internal/fetch"
	"github.com/cairnobs/cairnobs/internal/libvirt"
	"github.com/cairnobs/cairnobs/internal/netmap"
	"github.com/cairnobs/cairnobs/internal/nutanix"
	"github.com/cairnobs/cairnobs/internal/proxmox"
	"github.com/cairnobs/cairnobs/internal/purerest"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/tlsscan"
	"github.com/cairnobs/cairnobs/internal/units"
	"github.com/cairnobs/cairnobs/internal/version"
	"github.com/cairnobs/cairnobs/internal/vsphere"
	"github.com/cairnobs/cairnobs/internal/watch"
)

type config struct {
	Server        string        `yaml:"server"`         // obs-beacond's agent listener, e.g. https://10.0.0.3:8461
	CAFingerprint string        `yaml:"ca_fingerprint"` // the agent CA, pinned (`obs-beacond ca` prints it); needed with https
	TokenFile     string        `yaml:"token_file"`     // this agent's token: enrolls over https, signs every request over http
	StateDir      string        `yaml:"state_dir"`      // the agent's certificate and key; default /var/lib/obs-beacon-agent
	Host          string        `yaml:"host"`           // defaults to the hostname
	Interval      time.Duration `yaml:"interval"`       // defaults to 15s
	HelperSocket  string        `yaml:"helper_socket"`  // empty: no containers and no map sample
	BackupScan    string        `yaml:"backup_scan"`    // the backup scan's output; empty: none
	// Map: false stops the map sample (docs/maps.md) on this host: no socket
	// sample and no WireGuard read. Default true when there's a helper.
	Map *bool `yaml:"map"`
	// CertScan: false stops trying a TLS handshake on each listening port
	// once a day for the certificate inventory (docs/cert-inventory.md).
	// Default true when the map sample is on: the ports come from it.
	CertScan *bool `yaml:"cert_scan"`
	// CertFiles are globs of certificate files the root helper reads for
	// the inventory (step 3): absent, certfiles.Defaults; [] reads none.
	// Only the certificates come back, never a key.
	CertFiles *[]string `yaml:"cert_files"`
	// CertReload lists the units obs-beacond may ask this agent to reload
	// when one serves an older certificate than its renewed file
	// (docs/cert-rotation.md). The root helper reads it from this file
	// too. Absent: nothing is ever reloaded.
	CertReload []report.ReloadRule `yaml:"cert_reload"`
	// Who watches Cairn OBS: alert through this host's own channels when
	// obs-beacond is gone. Set on hosts other than obs-beacond's.
	WatchServer *watch.Config `yaml:"watch_server"`
	// On one node of a Proxmox VE cluster: read the whole cluster's VMs,
	// containers and nodes with a PVEAuditor token (docs/cluster-watcher.md).
	Proxmox *proxmox.Config `yaml:"proxmox"`
	// A vCenter this host can reach: read its VMs, ESXi hosts, datastores
	// and alarms as a Read-only user (docs/cluster-watcher.md).
	VSphere *vsphere.Config `yaml:"vsphere"`
	Nutanix *nutanix.Config `yaml:"nutanix"`
	// Pure is the FlashArrays and FlashBlades this host reads
	// (docs/pure.md): any number.
	Pure []purerest.Config `yaml:"pure"`
}

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 && os.Args[1] == "scan" {
		if err := runScan(os.Args[2:]); err != nil {
			log.Fatalf("obs-beacon-agent scan: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "join" {
		if err := runJoin(os.Args[2:]); err != nil {
			log.Fatalf("obs-beacon-agent join: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "cluster" {
		if err := runCluster(os.Args[2:]); err != nil {
			log.Fatalf("obs-beacon-agent cluster: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "helper" {
		if err := runHelper(os.Args[2:]); err != nil {
			log.Fatalf("obs-beacon-agent helper: %v", err)
		}
		return
	}
	cfgPath := flag.String("config", "/etc/obs-beacon/agent.yaml", "config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("obs-beacon-agent: %v", err)
	}
	if err := run(cfg); err != nil {
		log.Fatalf("obs-beacon-agent: %v", err)
	}
}

func loadConfig(p string) (*config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Server == "" {
		return nil, fmt.Errorf("%s: server is required", p)
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	c.Server = strings.TrimRight(c.Server, "/")
	if c.Interval == 0 {
		c.Interval = 15 * time.Second
	}
	if c.Interval < 5*time.Second {
		return nil, fmt.Errorf("%s: interval must be at least 5s", p)
	}
	if err := certreload.Validate(c.CertReload); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Host == "" {
		if c.Host, err = os.Hostname(); err != nil {
			return nil, err
		}
		c.Host, _, _ = strings.Cut(c.Host, ".")
	}
	return &c, nil
}

type agent struct {
	cfg    *config
	link   *agentlink.Link
	coll   collect.Collector
	docker *docker.Client
	// sources are the names of the platforms it reads, for its reports
	sources []string
	queue   *queue
	// the SMBIOS UUID from the helper, once it has answered
	machine      string
	machineKnown bool
	machineAsked time.Time
	seen         map[string]bool // problems already logged, so a broken source logs once
	down         bool            // obs-beacond unreachable since the last success
	jitter       time.Duration   // the longest wait before sending a backlog (retryJitter)

	watch      *watch.Watch        // nil unless watch_server is set
	logs       map[string]logsSeen // by container id: the last log read, for containers in trouble
	checks     *check.Runner
	units      []string     // systemd units obs-beacond asked for
	services   []string     // services running, for adopting in the UI
	servicesAt time.Time    // when they were last listed; zero: never
	noSystemd  bool         // systemctl isn't there: stop asking
	fetches    []fetch.Spec // status pages obs-beacond asked for
	checksHave string       // version of the check list running
	checksWant string       // version obs-beacond last asked for (X-Beacon-Checks)

	actionsWant bool                  // obs-beacond has actions waiting (X-Beacon-Actions)
	results     []report.ActionResult // what came of actions taken, for the next report

	certs   *tlsscan.Scanner // nil when cert_scan is off
	files   *certfiles.Watch // nil when cert_files is []
	mapAcc  *report.Map      // map samples since the last one sent, merged
	mapSent time.Time        // when a map sample last went with a report

	scan     *report.Backups // the backup scan last read, and its file's time
	scanTime time.Time

	vmPrev  map[string]libvirt.Domain // the last libvirt reading, by UUID, for CPU use
	vmSince map[string]vmSeen         // when each VM's state was first seen as it is
}

type vmSeen struct {
	word  string
	since time.Time
}

// readVMs reads the host's libvirt domains through the helper: nil where
// there's no libvirt, so the report has no vms field.
func (a *agent) readVMs(ctx context.Context) *report.VMs {
	vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	doms, err := a.docker.VMs(vctx)
	cancel()
	switch {
	case errors.Is(err, libvirt.ErrAbsent):
		a.vmPrev = nil
		return nil
	case err != nil:
		a.once("vms:"+err.Error(), "libvirt: %v", err)
		return &report.VMs{Error: err.Error()}
	}
	recs, use := libvirt.Records(doms, a.vmPrev)
	a.vmPrev = make(map[string]libvirt.Domain, len(doms))
	for _, d := range doms {
		a.vmPrev[d.UUID] = d
	}
	// libvirt doesn't say since when: it's when this agent first saw it so.
	now := time.Now().UTC().Truncate(time.Second)
	since := make(map[string]vmSeen, len(recs))
	for i, r := range recs {
		s, ok := a.vmSince[r.ID]
		if !ok || s.word != r.Word {
			s = vmSeen{word: r.Word, since: now}
		}
		since[r.ID], recs[i].Since = s, s.since
	}
	a.vmSince = since
	return &report.VMs{List: recs, Usage: use}
}

func run(cfg *config) error {
	link, err := agentlink.New(agentlink.Config{Server: cfg.Server, CAFingerprint: cfg.CAFingerprint, TokenFile: cfg.TokenFile, StateDir: cfg.StateDir})
	if err != nil {
		return err
	}
	if !link.TLS() {
		log.Printf("reporting over plain http: only safe inside a VPN; use https and ca_fingerprint")
	}
	a := &agent{cfg: cfg, link: link, seen: map[string]bool{}, jitter: retryJitter(cfg.Interval)}
	a.queue = openQueue(filepath.Join(cfg.StateDir, "queue"), maxQueue*cfg.Interval, time.Now())
	if n := a.queue.len(); n > 0 {
		log.Printf("%d reports from before the restart are waiting to go", n)
	}
	if cfg.HelperSocket != "" {
		a.docker = docker.NewClient(cfg.HelperSocket)
	}
	if cfg.WatchServer != nil {
		if a.watch, err = watch.New(*cfg.WatchServer, cfg.Host, time.Now()); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.checks = check.NewRunner(ctx)
	if ns, err := platformSetups(cfg); err != nil {
		log.Print(err)
		a.sources = []string{}
	} else {
		a.sources = sourceNames(ns)
		for _, n := range ns {
			go a.runPlatform(ctx, n)
		}
	}

	if a.docker != nil && (cfg.Map == nil || *cfg.Map) && (cfg.CertScan == nil || *cfg.CertScan) {
		a.certs = tlsscan.New(cfg.Host)
		go func() {
			t := time.NewTicker(tlsscan.RoundEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					a.certs.Round(ctx, now)
				}
			}
		}()
	}

	if globs := certfiles.Defaults; a.docker != nil && (cfg.CertFiles == nil || len(*cfg.CertFiles) > 0) {
		if cfg.CertFiles != nil {
			globs = *cfg.CertFiles
		}
		a.files = &certfiles.Watch{}
		go func() {
			logged := "" // its own goroutine: a.once is the tick's
			for {
				fctx, cancel := context.WithTimeout(ctx, time.Minute)
				list, err := a.docker.CertFiles(fctx, globs)
				cancel()
				if err != nil {
					if err.Error() != logged {
						logged = err.Error()
						log.Printf("certificate files: %v", err)
					}
				} else {
					a.files.Set(list)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(certfiles.AskEvery):
				}
			}
		}()
	}

	log.Printf("obs-beacon-agent %s: host %s, reporting to %s every %s", version.Version, cfg.Host, cfg.Server, cfg.Interval)
	// The first sample only primes the CPU and network counters.
	a.coll.Collect(&report.Report{}, time.Now())
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			a.tick(ctx, now)
		}
	}
}

func (a *agent) once(key, format string, args ...any) {
	if !a.seen[key] {
		a.seen[key] = true
		log.Printf(format, args...)
	}
}

func (a *agent) tick(ctx context.Context, now time.Time) {
	r := report.Report{
		V: report.Version, Agent: version.Version, Host: a.cfg.Host,
		Time: now.UTC(), Interval: int(a.cfg.Interval / time.Second),
		Sources: a.sources,
	}
	for _, err := range a.coll.Collect(&r, now) {
		a.once("collect:"+err.Error(), "collect: %v", err)
	}
	if a.docker != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		list, err := a.docker.Containers(cctx)
		cancel()
		switch {
		case errors.Is(err, docker.ErrNoDocker):
			// a helper on a host without Docker: no containers field
		case err != nil:
			r.Containers = &report.Containers{Error: err.Error()}
			a.once("docker:"+err.Error(), "containers: %v", err)
		default:
			a.attachLogs(ctx, list, now)
			r.Containers = &report.Containers{List: list}
		}
		// The machine's UUID doesn't change while the agent runs: asked for
		// until the helper answers, then kept.
		if !a.machineKnown && now.Sub(a.machineAsked) >= 10*time.Minute {
			a.machineAsked = now
			mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			u, err := a.docker.Machine(mctx)
			cancel()
			if err != nil {
				a.once("machine:"+err.Error(), "machine UUID: %v", err)
			} else {
				a.machine, a.machineKnown = u, true
			}
		}
		r.Machine = a.machine
		r.VMs = a.readVMs(ctx)
		if a.cfg.Map == nil || *a.cfg.Map {
			mctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			m, err := a.docker.Map(mctx)
			cancel()
			if err != nil {
				a.once("map:"+err.Error(), "map sample: %v", err)
				m = &report.Map{Error: err.Error()}
			}
			if r.Containers != nil {
				nameContainers(m, r.Containers.List)
			}
			if a.certs != nil && m.Error == "" {
				a.certs.Update(m.Listen, now)
			}
			a.mapAcc = netmap.Merge(a.mapAcc, m)
			if now.Sub(a.mapSent) >= netmap.SendEvery-a.cfg.Interval/2 {
				r.Map, a.mapAcc, a.mapSent = a.mapAcc, nil, now
			}
		}
	}
	if len(a.units) > 0 {
		uctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		us, err := units.Read(uctx, a.units)
		cancel()
		if err != nil {
			a.once("units:"+err.Error(), "units: %v", err)
		}
		r.Units = us
	}
	r.Services = a.runningServices(ctx, now)
	for _, f := range a.fetches {
		fctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		r.Fetches = append(r.Fetches, fetch.Get(fctx, f))
		cancel()
	}
	if a.certs != nil {
		r.Certs = a.certs.Report(now)
	}
	if a.files != nil {
		r.CertFiles = a.files.Report(now)
	}
	r.Checks = a.checks.Results()
	r.Backups = a.backupScan()
	r.Reloads = a.reloadRules()
	r.Actions, a.results = a.results, nil
	b, err := json.Marshal(&r)
	if err != nil {
		log.Printf("encode: %v", err)
		return
	}
	a.queue.push(now, b)
	_ = a.link.Maintain(ctx, now) // enroll or renew when due; logs its own problems
	if a.down || a.queue.len() > 1 {
		a.wait(ctx)
	}
	a.flush(ctx)
	a.queue.persist() // whatever didn't go out, to disk
	if a.watch != nil {
		if a.down {
			a.watch.Failed(ctx, now)
		} else {
			a.watch.Reported(ctx, now)
		}
	}
	if a.checksWant != a.checksHave {
		a.fetchChecks(ctx)
	}
	if a.actionsWant {
		a.actionsWant = false
		a.takeActions(ctx)
	}
}

// nameContainers puts container names in place of the short IDs the
// helper knows them by.
func nameContainers(m *report.Map, list []report.Container) {
	names := make(map[string]string, len(list))
	for _, c := range list {
		if len(c.ID) >= 12 {
			names["container:"+c.ID[:12]] = "container:" + c.Name
		}
	}
	for i, l := range m.Listen {
		if n, ok := names[l.Owner]; ok {
			m.Listen[i].Owner = n
		}
	}
	for i, c := range m.Conns {
		if n, ok := names[c.Owner]; ok {
			m.Conns[i].Owner = n
		}
	}
}

// runningServices lists the host's running services every five minutes:
// often enough for the adopt page, rarely enough to cost nothing.
func (a *agent) runningServices(ctx context.Context, now time.Time) []string {
	if a.noSystemd || now.Sub(a.servicesAt) < 5*time.Minute {
		return a.services
	}
	a.servicesAt = now
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	list, err := units.Running(sctx)
	if errors.Is(err, exec.ErrNotFound) {
		a.noSystemd = true
		a.services = nil
		return nil
	}
	if err != nil {
		a.once("services:"+err.Error(), "services: %v", err)
		return a.services // the last good list
	}
	a.services = list
	return list
}

// fetchChecks asks obs-beacond for this host's checks after a report's answer
// said the list changed. A failure keeps the old list and tries again
// after the next report.
func (a *agent) fetchChecks(ctx context.Context) {
	if a.checksWant == "" { // obs-beacond has no checks for this host
		a.checks.Set(nil)
		a.units = nil
		a.fetches = nil
		a.checksHave = ""
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+"/api/v1/agent/checks", nil)
	if err != nil {
		return
	}
	a.link.Authorize(req)
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	resp, err := a.link.Client().Do(req)
	if err != nil {
		a.once("checks:"+err.Error(), "can't fetch checks: %v", err)
		return
	}
	defer resp.Body.Close()
	var list struct {
		Version string       `json:"version"`
		Checks  []check.Spec `json:"checks"`
		Units   []string     `json:"units"`
		Fetches []fetch.Spec `json:"fetches"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil {
		a.once(fmt.Sprintf("checks:%d", resp.StatusCode), "can't fetch checks: HTTP %d", resp.StatusCode)
		return
	}
	var good []check.Spec
	for _, s := range list.Checks {
		if err := s.Check(); err != nil {
			log.Printf("skipping a check obs-beacond sent: %v", err)
			continue
		}
		good = append(good, s)
	}
	a.checks.Set(good)
	a.units = list.Units
	a.fetches = a.fetches[:0]
	for _, f := range list.Fetches {
		if err := f.Check(); err != nil {
			log.Printf("skipping a status page obs-beacond sent: %v", err)
			continue
		}
		a.fetches = append(a.fetches, f)
	}
	a.checksHave = list.Version
	log.Printf("running %d checks, watching %d units, reading %d status pages (list %s)", len(good), len(list.Units), len(a.fetches), list.Version)
}

// retryJitter is the longest an agent waits, at random, before sending
// after obs-beacond was out of reach (or with reports kept from before a
// restart): when obs-beacond comes back, or a fleet's agents are upgraded
// together, they come back spread over that time instead of all in the
// same second. Half the interval, at most 10 s.
func retryJitter(interval time.Duration) time.Duration {
	return min(interval/2, 10*time.Second)
}

// wait sleeps for a random part of a.jitter, or until ctx ends.
func (a *agent) wait(ctx context.Context) {
	if a.jitter <= 0 {
		return
	}
	t := time.NewTimer(rand.N(a.jitter))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// flush sends queued reports oldest first and stops at the first failure.
// All but the newest go as queued, so obs-beacond takes them as history
// rather than as the host's state now.
func (a *agent) flush(ctx context.Context) {
	if !a.link.Ready() { // not enrolled yet: keep everything for later
		a.down = true
		return
	}
	for a.queue.len() > 0 {
		body, err := a.queue.peek()
		if err != nil {
			log.Printf("dropping a queued report: %v", err)
			a.queue.pop()
			continue
		}
		err = a.send(ctx, body, a.queue.len() > 1)
		var reject rejected
		switch {
		case err == nil:
			a.queue.pop()
			if a.down {
				a.down = false
				log.Printf("obs-beacond reachable again")
			}
		case errors.As(err, &reject) && (reject.code == http.StatusBadRequest || reject.code == http.StatusRequestEntityTooLarge):
			// Sending it again can't succeed; don't let it block the queue.
			log.Printf("obs-beacond refused a report, dropping it: %v", err)
			a.queue.pop()
		default:
			if errors.As(err, &reject) && reject.code == http.StatusUnauthorized {
				a.link.Refused()
			}
			if !a.down {
				a.down = true
				log.Printf("can't report, keeping up to %d reports: %v", maxQueue, err)
			}
			return
		}
	}
}

type rejected struct {
	code int
	msg  string
}

func (r rejected) Error() string { return fmt.Sprintf("%d %s", r.code, r.msg) }

func (a *agent) send(ctx context.Context, body []byte, queued bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Server+"/api/v1/agent/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	a.link.Authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	if queued {
		req.Header.Set("X-Beacon-Queued", "1")
	}
	resp, err := a.link.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		a.checksWant = resp.Header.Get("X-Beacon-Checks")
		a.actionsWant = a.actionsWant || resp.Header.Get("X-Beacon-Actions") != ""
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return rejected{code: resp.StatusCode, msg: strings.TrimSpace(string(msg))}
}

// runHelper serves the read-only Docker gate on a unix socket. It runs as
// root (it needs the Docker socket); the agent reaches it through the
// socket's group.
func runHelper(args []string) error {
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	listen := fs.String("listen", "/run/obs-beacon/helper.sock", "unix socket to serve on")
	dockerSock := fs.String("docker", "/var/run/docker.sock", "Docker's socket")
	group := fs.String("group", "obs-beacon", "group allowed to use the socket (empty: owner only)")
	config := fs.String("config", "/etc/obs-beacon/agent.yaml", "the agent's config, read for cert_reload (the agent's word isn't taken)")
	_ = fs.Parse(args)

	_ = os.Remove(*listen)
	ln, err := net.Listen("unix", *listen)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if *group != "" {
		g, err := user.LookupGroup(*group)
		if err != nil {
			return err
		}
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(*listen, -1, gid); err != nil {
			return err
		}
		mode = 0o660
	}
	if err := os.Chmod(*listen, mode); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", docker.Gate(*dockerSock))
	var sampling sync.Mutex
	sampler := &netmap.Sampler{}
	mux.HandleFunc("GET "+docker.MapPath, func(w http.ResponseWriter, r *http.Request) {
		sampling.Lock() // one sample at a time
		m := sampler.Sample()
		sampling.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	// Only root may read it; it says nothing a guest can't see.
	mux.HandleFunc("GET "+docker.MachinePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile("/sys/class/dmi/id/product_uuid")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"uuid": report.MachineUUID(string(b))})
	})
	// libvirt's read-only socket: domains and their numbers, nothing more.
	var reading sync.Mutex
	mux.HandleFunc("GET "+docker.VMsPath, func(w http.ResponseWriter, r *http.Request) {
		reading.Lock()
		doms, err := libvirt.Read(libvirt.Sockets, "qemu:///system", 10*time.Second)
		reading.Unlock()
		out := map[string]any{"domains": doms}
		switch {
		case errors.Is(err, libvirt.ErrAbsent):
			out = map[string]any{"absent": true}
		case err != nil:
			out = map[string]any{"error": err.Error()}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	// Certificate files: the certificates parsed out of the files the
	// agent's globs match, never a key, and nothing for a file without a
	// certificate in it.
	files := &certfiles.Reader{}
	mux.HandleFunc("GET "+docker.CertFilesPath, func(w http.ResponseWriter, r *http.Request) {
		globs := r.URL.Query()["glob"]
		if len(globs) > 64 {
			globs = globs[:64]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files.Read(globs)})
	})
	// The one thing it changes: reloading a unit for a renewed certificate,
	// only as agent.yaml's cert_reload allows, read here on each request
	// (docs/cert-rotation.md, step 2). One at a time.
	var reloading sync.Mutex
	mux.HandleFunc("POST "+docker.ReloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req certreload.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad request: " + err.Error()})
			return
		}
		reloading.Lock()
		defer reloading.Unlock()
		rules, err := certreload.ReadRules(*config)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "can't read cert_reload: " + err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		res, err := certreload.Do(ctx, rules, req, certreload.Systemd{}, time.Now())
		out := map[string]any{"result": res}
		if err != nil {
			out["error"] = err.Error()
			log.Printf("reload %s refused: %v", req.Unit, err)
		} else {
			log.Printf("reloaded %s for %s (%s)", req.Unit, res.File, res.Fingerprint)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("obs-beacon-agent helper %s: read-only Docker gate, map sample, libvirt domains, certificate files and cert_reload on %s", version.Version, *listen)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type logsSeen struct {
	status string
	at     time.Time
	lines  []string
}

// logLines is how much of a troubled container's log goes with a report.
const logLines = 20

// troubled says whether a container's last log lines are worth sending:
// it's restarting, unhealthy or dead, or it exited within the last day.
func troubled(c report.Container) bool {
	switch {
	case c.State == "restarting", c.State == "dead", c.Health == "unhealthy":
		return true
	case c.State == "exited":
		s := c.Status
		return strings.Contains(s, "second") || strings.Contains(s, "minute") || strings.Contains(s, "hour")
	}
	return false
}

// attachLogs adds the last lines to troubled containers. A log is read
// again when the container's status changes, or after two minutes, and at
// most ten are read per report.
func (a *agent) attachLogs(ctx context.Context, list []report.Container, now time.Time) {
	if a.logs == nil {
		a.logs = map[string]logsSeen{}
	}
	keep := map[string]bool{}
	reads := 0
	for i := range list {
		c := &list[i]
		if !troubled(*c) {
			continue
		}
		keep[c.ID] = true
		seen, ok := a.logs[c.ID]
		if (!ok || seen.status != c.Status || now.Sub(seen.at) > 2*time.Minute) && reads < 10 {
			reads++
			lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			lines, err := a.docker.Logs(lctx, c.ID, logLines)
			cancel()
			if err != nil {
				a.once("logs:"+err.Error(), "container logs: %v", err)
			} else {
				seen = logsSeen{status: c.Status, at: now, lines: lines}
				a.logs[c.ID] = seen
			}
		}
		c.Logs = seen.lines
	}
	for id := range a.logs {
		if !keep[id] {
			delete(a.logs, id)
		}
	}
}
