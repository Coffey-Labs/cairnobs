package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The demo's own integrations (`fleet config -demo`, `fleet run -demo`):
// the checks every site runs, the units and containers each host must
// keep running, and the backups each site keeps, so that every page has
// something to show. config writes the files obs-beacond reads; run makes
// the hosts report against them. Both work them out from the number of
// agents, so give both the same -n. Problems on purpose, most of them
// coming and going (scenario.go):
//
//	checks    legacy.example.com answers 502 now and then (a warning-only
//	          check); billing.example.com's certificate ends in 9 days;
//	          the ping to the last site loses 3%; mx2 lands on a block
//	          list now and then
//	units     phx1-cache02's redis-server fails now and then, and is
//	          restarted
//	containers phx1-app05's app-04 exits now and then; phx1-app06's app-09
//	          is stopped on purpose
//	backups   site 1's pg-dump is late now and then; site 2's shrinks to a
//	          third now and then; site 3's backup disk is 93% full; site
//	          4's pg-backup job failed; site 5's md1 is missing a disk

// role is what a host does in its site, by position (netmap.go).
func role(pos int) string {
	switch {
	case pos == 0:
		return "gateway"
	case pos <= 2:
		return "lb"
	case pos <= 29:
		return "app"
	case pos <= 39:
		return "db"
	case pos <= 44:
		return "cache"
	}
	return "mq"
}

// demoUnits are the systemd units host n must keep active.
func demoUnits(n int) []string {
	base := []string{"ssh", "chrony"} // Debian's and Ubuntu's names
	if strings.HasPrefix(distroFor(n).os, "Red Hat") {
		base = []string{"sshd", "chronyd"}
	}
	switch role(n % siteSize) {
	case "gateway":
		return append(base, "wg-quick@wg0")
	case "lb":
		return append(base, "haproxy")
	case "app":
		return append(base, "docker")
	case "db":
		return append(base, "postgresql")
	case "cache":
		return append(base, "redis-server")
	}
	return append(base, "nats-server")
}

const (
	demoFailedUnit    = 41 // its redis-server has failed
	demoExited        = 7  // its app-04 exited
	demoStoppedOnPurp = 8  // its app-09 is stopped on purpose
)

// demoCheck is one check, where it runs from, and how it goes.
type demoCheck struct {
	id, name, yaml string // yaml: the check's own lines, after id and name
	from           []int
	result         func(from int, now time.Time) report.CheckResult
}

func gateways(hosts int) []int {
	var g []int
	for s := range (hosts + siteSize - 1) / siteSize {
		g = append(g, s*siteSize)
	}
	return g
}

func demoChecks(hosts int) []demoCheck {
	gws := gateways(hosts)
	web := gws[:min(3, len(gws))]
	ok := func(id string, ms float64, f func(*report.CheckResult)) func(int, time.Time) report.CheckResult {
		return func(from int, now time.Time) report.CheckResult {
			r := report.CheckResult{ID: id, At: now.UTC(), OK: true, Ms: ms * (0.8 + 0.4*float64(from%7)/6)}
			if f != nil {
				f(&r)
			}
			return r
		}
	}
	cert := func(days int) func(*report.CheckResult) {
		return func(r *report.CheckResult) {
			r.Status = 200
			r.CertExpires = time.Now().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Hour).UTC()
		}
	}
	var out []demoCheck
	for _, w := range []struct {
		id, host string
		days     int
	}{{"web-shop", "shop.example.com", 61}, {"web-api", "api.example.com", 61}, {"web-docs", "docs.example.com", 74}, {"web-status", "status.example.com", 45}, {"web-billing", "billing.example.com", 9}} {
		out = append(out, demoCheck{id: w.id, name: w.host, yaml: fmt.Sprintf("http: https://%s/", w.host), from: web, result: ok(w.id, 120, cert(w.days))})
	}
	out = append(out,
		demoCheck{id: "web-legacy", name: "legacy.example.com", yaml: "http: https://legacy.example.com/\n    severity: warning", from: web,
			result: func(from int, now time.Time) report.CheckResult {
				if !spellLegacy502.active(now) {
					return report.CheckResult{ID: "web-legacy", At: now.UTC(), OK: true, Status: 200, Ms: 340}
				}
				return report.CheckResult{ID: "web-legacy", At: now.UTC(), Status: 502, Ms: 2100, Error: "HTTP 502, want 200"}
			}},
		demoCheck{id: "dns-mx", name: "example.com MX", yaml: "dns: example.com\n    type: MX\n    contains: mx1.example.com", from: web,
			result: ok("dns-mx", 12, func(r *report.CheckResult) { r.Answer = "10 mx1.example.com." })},
		demoCheck{id: "smtp", name: "mx1.example.com SMTP", yaml: "tcp: mx1.example.com:25\n    contains: ESMTP", from: web,
			result: ok("smtp", 35, func(r *report.CheckResult) { r.Answer = "220 mx1.example.com ESMTP ready" })},
		demoCheck{id: "submission", name: "mail.example.com submission", yaml: "tcp: mail.example.com:587\n    contains: ESMTP", from: web,
			result: ok("submission", 38, func(r *report.CheckResult) { r.Answer = "220 mail.example.com ESMTP ready" })},
		demoCheck{id: "imaps", name: "mail.example.com IMAPS", yaml: "tcp: mail.example.com:993", from: web, result: ok("imaps", 41, nil)},
		demoCheck{id: "rbl-mx1", name: "mx1.example.com on block lists", yaml: "rbl: 192.0.2.25", from: web[:1],
			result: ok("rbl-mx1", 180, func(r *report.CheckResult) { r.Answer = "clean on 5 of 5" })},
		// mx2 landed on a list that flags whole networks: a warning to look at, not a page.
		demoCheck{id: "rbl-mx2", name: "mx2.example.com on block lists", yaml: "rbl: 192.0.2.26\n    severity: warning", from: web[:1],
			result: func(from int, now time.Time) report.CheckResult {
				if !spellBlocklisted.active(now) {
					return report.CheckResult{ID: "rbl-mx2", At: now.UTC(), OK: true, Ms: 190, Answer: "clean on 5 of 5"}
				}
				return report.CheckResult{ID: "rbl-mx2", At: now.UTC(), Ms: 210, Answer: "clean on 4 of 5",
					Error: "listed on dnsbl-1.uceprotect.net (127.0.0.2)"}
			}},
	)
	// Every site's gateway pings every other's over WireGuard.
	for s, gw := range gws {
		var from []int
		for _, o := range gws {
			if o != gw {
				from = append(from, o)
			}
		}
		if len(from) == 0 {
			continue
		}
		id := fmt.Sprintf("ping-site-%02d", s)
		last := s == len(gws)-1 && s > 0
		out = append(out, demoCheck{id: id, name: fmt.Sprintf("site %d over WireGuard", s), yaml: "ping: " + siteAddr(s, 0) + "\n    severity: warning", from: from,
			result: func(from int, now time.Time) report.CheckResult {
				d := from/siteSize - s
				if d < 0 {
					d = -d
				}
				r := report.CheckResult{ID: id, At: now.UTC(), OK: true, Ms: 0.4 + 9.5*float64(d)}
				if last {
					r.Loss = 3
				}
				return r
			}})
	}
	return out
}

// demoBackups: in each of the first six sites the gateway keeps the
// backups of the site's first four databases.
type demoBackup struct {
	store   int
	senders []int
}

func demoBackupSites(hosts int) []demoBackup {
	var out []demoBackup
	for s := range min(6, (hosts+siteSize-1)/siteSize) {
		b := demoBackup{store: s * siteSize}
		for p := 30; p < 34; p++ {
			if s*siteSize+p < hosts {
				b.senders = append(b.senders, s*siteSize+p)
			}
		}
		if len(b.senders) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// demoScan is host n's backup scan, or nil.
func demoScan(n, hosts int, now time.Time) *report.Backups {
	for _, b := range demoBackupSites(hosts) {
		site := b.store / siteSize
		if n == b.store {
			sc := &report.Backups{At: now.Add(-7 * time.Minute).UTC(), Jobs: []report.BackupJob{}, Made: []report.BackupDir{},
				RAID:  []report.RAIDArray{{Dev: "md0", State: "[UU]"}, {Dev: "md1", State: "[UU]"}},
				SMART: []report.DiskHealth{{Disk: "sda", OK: true}, {Disk: "sdb", OK: true}}}
			used := 41 + float64(site*7)
			if site == 3 {
				used = 93
			}
			if site == 5 {
				sc.RAID[1].State = "[U_]"
			}
			sc.Roots = []report.BackupRoot{{Path: "/srv/backups", UsedPct: used}}
			for k, from := range b.senders {
				for _, name := range []string{"pg-dump", "system"} {
					size := int64(3+k) << 30
					if name == "system" {
						size = int64(800+40*k) << 20
					}
					d := report.BackupDir{Name: name, From: hostName(from), Size: size, PrevSize: size - size/50, Count: 14,
						Modified: now.Add(-time.Duration(3+k) * time.Hour).UTC()}
					if k == 0 && name == "pg-dump" {
						switch {
						case site == 1 && spellLateBackup.active(now):
							// Last night's never came: the newest is the night before's.
							d.Modified = spellLateBackup.began(now).Add(-26*time.Hour - 10*time.Minute).UTC()
						case site == 2 && spellBackupShrank.active(now):
							d.PrevSize, d.Size = d.Size, d.Size/3
						}
					}
					// Each copy is in a folder named after the day it was made.
					d.Path = fmt.Sprintf("/srv/backups/%s/%s/%s", hostName(from), name, d.Modified.Format("2006-01-02"))
					sc.Received = append(sc.Received, d)
				}
			}
			return sc
		}
		for k, from := range b.senders {
			if n != from {
				continue
			}
			j := report.BackupJob{Unit: "pg-backup.service", Result: "success", Active: "inactive", Finished: now.Add(-time.Duration(3+k) * time.Hour).UTC()}
			if site == 4 && k == 0 {
				j.Result = "exit-code"
			}
			return &report.Backups{At: now.Add(-5 * time.Minute).UTC(), Received: []report.BackupDir{}, Made: []report.BackupDir{}, Roots: []report.BackupRoot{},
				Jobs: []report.BackupJob{j}, RAID: []report.RAIDArray{}, SMART: []report.DiskHealth{{Disk: "nvme0n1", OK: true}}}
		}
	}
	return nil
}

// writeDemo writes the demo's files into dir, and demo.yaml: the lines to
// add to obs-beacond.yaml.
func writeDemo(dir string, hosts, windows int) {
	var checks bytes.Buffer
	checks.WriteString("# The fleet demo's checks (fleet config -demo).\nchecks:\n")
	for _, c := range demoChecks(hosts) {
		fmt.Fprintf(&checks, "  - id: %s\n    name: %q\n    %s\n    from: [", c.id, c.name, c.yaml)
		for k, f := range c.from {
			if k > 0 {
				checks.WriteString(", ")
			}
			checks.WriteString(hostName(f))
		}
		checks.WriteString("]\n")
	}
	var exp bytes.Buffer
	exp.WriteString("# The fleet demo's hosts: what must keep running (fleet config -demo).\nhosts:\n")
	for n := range hosts {
		fmt.Fprintf(&exp, "  %s:\n    running: [\"app-*\"]\n    units: [", hostName(n))
		for k, u := range demoUnits(n) {
			if k > 0 {
				exp.WriteString(", ")
			}
			exp.WriteString(u)
		}
		exp.WriteString("]\n")
		if n == demoStoppedOnPurp {
			exp.WriteString("    stopped:\n      - match: app-09\n        note: Replaced by app-09b; kept for a rollback until Friday.\n")
		}
	}
	exp.WriteString(winExpected(windows))
	{
		// The IBM i partitions' subsystems that must be active (run -ibmi):
		// prod2's ORDERS isn't, and ha1, the standby, expects none.
		src := ibmiCollector.name
		fmt.Fprintf(&exp, "clusters:\n  %s:prod1:\n    subsystems: [QINTER, QBATCH, QUSRWRK, ORDLIB/ORDERS]\n", src)
		fmt.Fprintf(&exp, "  %s:prod2:\n    subsystems: [QINTER, QBATCH, QUSRWRK, ORDLIB/ORDERS]\n", src)
		fmt.Fprintf(&exp, "  %s:dev1:\n    subsystems: [QINTER, QBATCH]\n", src)
	}
	var bk bytes.Buffer
	bk.WriteString("# The fleet demo's backups (fleet config -demo).\nmax_age: 26h\nhosts:\n")
	for _, b := range demoBackupSites(hosts) {
		fmt.Fprintf(&bk, "  %s:\n    received:\n", hostName(b.store))
		for _, s := range b.senders {
			fmt.Fprintf(&bk, "      - {from: %s, root: /srv/backups/%s, expect: [pg-dump, system]}\n", hostName(s), hostName(s))
		}
		for _, s := range b.senders {
			fmt.Fprintf(&bk, "  %s:\n    jobs: [pg-backup.service]\n", hostName(s))
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "topology.yaml"), []byte(demoTopology), 0o644); err != nil {
		log.Fatal(err)
	}
	conf := fmt.Sprintf("# The fleet demo (fleet config -demo): add these to obs-beacond.yaml.\nchecks: %s\nexpected: %s\nbackups: %s\ntopology: %s\n",
		filepath.Join(abs, "checks.yaml"), filepath.Join(abs, "expected.yaml"), filepath.Join(abs, "backups.yaml"), filepath.Join(abs, "topology.yaml"))
	if hasDataLayer(hosts) {
		conf += dataLayerYAML()
	}
	for name, b := range map[string][]byte{"checks.yaml": checks.Bytes(), "expected.yaml": exp.Bytes(), "backups.yaml": bk.Bytes(), "demo.yaml": []byte(conf)} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

// demoHost is set when the demo runs: what host n reports beside the rest.
type demoHost struct {
	n, hosts int
	checks   []demoCheck
}

func newDemoHost(n, hosts int, all []demoCheck) *demoHost {
	d := &demoHost{n: n, hosts: hosts}
	for _, c := range all {
		for _, f := range c.from {
			if f == n {
				d.checks = append(d.checks, c)
			}
		}
	}
	return d
}

// setup changes the host's containers for the demo's problems.
func (d *demoHost) setup(r *report.Report) {
	if r.Containers == nil {
		return
	}
	for k := range r.Containers.List {
		c := &r.Containers.List[k]
		if d.n == demoStoppedOnPurp && c.Name == "app-09" {
			c.State, c.Status, c.Health = "exited", "Exited (0) 2 days ago", ""
		}
	}
}

// containers puts the containers that come and go in their state now.
func (d *demoHost) containers(r *report.Report, now time.Time) {
	if d.n != demoExited || r.Containers == nil {
		return
	}
	list := append([]report.Container{}, r.Containers.List...)
	for k := range list {
		c := &list[k]
		if c.Name != "app-04" {
			continue
		}
		if o := spellAppExits.last(now); spellAppExits.active(now) {
			c.State, c.Status, c.Health = "exited", fmt.Sprintf("Exited (137) %s ago", fuzzy(now.Sub(o.start))), ""
		} else {
			c.State, c.Health = "running", "healthy"
			c.Status = fmt.Sprintf("Up %s (healthy)", fuzzy(now.Sub(o.end)))
		}
	}
	r.Containers = &report.Containers{List: list}
}

// fuzzy is a duration as docker ps says it.
func fuzzy(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "Less than a minute"
	case d < 2*time.Minute:
		return "About a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "About an hour"
	}
	return fmt.Sprintf("%d hours", int(d.Hours()))
}

// stamp adds the units, check results and backup scan to a report.
func (d *demoHost) stamp(r *report.Report, start, now time.Time) {
	r.Units = nil
	for _, u := range demoUnits(d.n) {
		un := report.Unit{Name: u + ".service", Load: "loaded", Active: "active", Sub: "running", Since: start.UTC()}
		if d.n == demoFailedUnit && u == "redis-server" {
			switch o := spellRedisFails.last(now); {
			case spellRedisFails.active(now):
				un.Active, un.Sub, un.Restarts, un.Since = "failed", "failed", 5, time.Time{}
			case o.start.After(start): // restarted by hand when the spell ended
				un.Since = o.end.UTC()
			}
		}
		r.Units = append(r.Units, un)
	}
	r.Checks = nil
	for _, c := range d.checks {
		r.Checks = append(r.Checks, c.result(d.n, now))
	}
	d.containers(r, now)
	r.Backups = demoScan(d.n, d.hosts, now)
	if hasDataLayer(d.hosts) {
		r.Fetches = dataFetches(d.n, now)
	}
}

// demoTopology names the demo's services: what each claims, what it needs
// (lines on the service map; alerts group by them) and the checks that
// judge it. The Mail page shows the mail service's checks.
const demoTopology = `# The fleet demo's services (fleet config -demo). Format: docs/formats.md, "topology.yaml".
services:
  - {id: cloudflare, name: Cloudflare, tier: edge, cloudflare: true}
  - {id: shop, name: Web shop, tier: public, needs: [cloudflare, apps], checks: [web-shop, web-billing]}
  - {id: api, name: Public API, tier: public, needs: [cloudflare, apps], checks: [web-api]}
  - {id: sites, name: Docs and status, tier: public, needs: [cloudflare], checks: [web-docs, web-status, web-legacy]}
  - {id: mail, name: Mail, tier: public, needs: [postgres], checks: [dns-mx, smtp, submission, imaps, rbl-mx1, rbl-mx2]}
  - {id: apps, name: App servers, tier: apps, needs: [haproxy, redis, nats], units: ["*/docker"], containers: ["*/app-*"]}
  - {id: haproxy, name: HAProxy, tier: data, needs: [postgres], units: ["*/haproxy"]}
  - {id: postgres, name: PostgreSQL (Patroni), tier: data, units: ["*/postgresql"]}
  - {id: redis, name: Redis, tier: data, units: ["*/redis-server"]}
  - {id: nats, name: NATS, tier: data, units: ["*/nats-server"]}
`
