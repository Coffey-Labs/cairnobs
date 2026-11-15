package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
	"github.com/cairnobs/cairnobs/internal/report"
)

const groupTopology = `services:
  - {id: webmail, name: Webmail, tier: public, needs: [inbuxa], checks: [webmail], containers: ["*/ihasmail"]}
  - {id: mail, name: Mail, tier: public, needs: [inbuxa], checks: [smtp-25, imap-993]}
  - {id: site, name: Websites, tier: public, needs: [nginx], checks: [site]}
  - {id: nginx, name: nginx, tier: apps, needs: [node1, node2], units: ["*/nginx"]}
  - {id: inbuxa, name: inbuxa server, tier: apps, needs: [patroni], units: ["*/inbuxa"]}
  - {id: patroni, name: PostgreSQL, tier: data, needs: [etcd], units: ["*/patroni"]}
  - {id: etcd, name: etcd, tier: data, needs: [node1, node2], units: ["*/etcd"]}
`

func groupServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := testServer(t, false)
	p := filepath.Join(t.TempDir(), "topology.yaml")
	_ = os.WriteFile(p, []byte(groupTopology), 0o644)
	topo, err := LoadTopology(p, srv.cfg.Agents)
	if err != nil {
		t.Fatal(err)
	}
	srv.topology.cur = topo
	return srv
}

func never(string) bool { return false }

var (
	pgDown   = Condition{Key: "unit/node1/patroni.service", Severity: "critical", Title: "patroni.service on node1: failed", Detail: "systemd reports it failed (failed).", Host: "node1"}
	smtpDown = Condition{Key: "check/smtp-25", Severity: "critical", Title: "SMTP is down", Detail: "Failing from node1, node2."}
	imapDown = Condition{Key: "check/imap-993", Severity: "critical", Title: "IMAP is down"}
	webmail  = Condition{Key: "container/node2/ihasmail", Severity: "warning", Title: "ihasmail on node2: unhealthy", Host: "node2"}
	siteSlow = Condition{Key: "check/site", Severity: "warning", Title: "Websites down"}
)

func TestOneIncidentPerRootCause(t *testing.T) {
	srv := groupServer(t)
	got := srv.groupConditions([]Condition{smtpDown, webmail, pgDown, imapDown, siteSlow}, never)
	if keys(got) != "critical:unit/node1/patroni.service warning:check/site" {
		t.Fatalf("got %s", keys(got))
	}
	pg := got[0]
	if pg.Affected != "Mail, Webmail" || !strings.Contains(pg.Detail, "Also affected: Mail, Webmail (3 alerts held back).") {
		t.Fatalf("root: %+v", pg)
	}
}

func TestRootTakesTheWorstSeverity(t *testing.T) {
	srv := groupServer(t)
	etcdWarn := Condition{Key: "unit/node2/etcd.service", Severity: "warning", Title: "etcd.service on node2: starting", Host: "node2"}
	got := srv.groupConditions([]Condition{etcdWarn, smtpDown}, never)
	if len(got) != 1 || got[0].Key != etcdWarn.Key || got[0].Severity != "critical" {
		t.Fatalf("got %+v", got)
	}
}

func TestAlreadyToldStaysOnItsOwn(t *testing.T) {
	srv := groupServer(t)
	told := func(k string) bool { return k == smtpDown.Key }
	got := srv.groupConditions([]Condition{smtpDown, pgDown, imapDown}, told)
	if keys(got) != "critical:check/smtp-25 critical:unit/node1/patroni.service" || got[1].Affected != "Mail" {
		t.Fatalf("got %s %+v", keys(got), got)
	}
}

func TestHostDownExplainsItsServices(t *testing.T) {
	srv := groupServer(t)
	silent := Condition{Key: "silent/node1", Severity: "critical", Title: "node1 has gone silent", Host: "node1"}
	nginx := Condition{Key: "unit/node2/nginx.service", Severity: "critical", Title: "nginx on node2: failed", Host: "node2"}
	// nginx needs node1 (and node2): node1 being silent is the deeper failure.
	got := srv.groupConditions([]Condition{nginx, siteSlow, silent}, never)
	if keys(got) != "critical:silent/node1" || got[0].Affected != "nginx, Websites" {
		t.Fatalf("got %s %+v", keys(got), got)
	}
}

func TestNoTopologyNoGrouping(t *testing.T) {
	srv, _ := testServer(t, false)
	if got := srv.groupConditions([]Condition{smtpDown, pgDown}, never); len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestGroupedIncidentEndToEnd(t *testing.T) {
	srv := groupServer(t)
	ch := &spy{id: "ops", min: "warning"}
	srv.alerts.channels = append(srv.alerts.channels[:0], ch)
	t0 := time.Now()
	for _, m := range []int{0, 3} {
		conds := srv.groupConditions([]Condition{smtpDown, pgDown, webmail}, srv.alerts.firing)
		srv.alerts.evaluate(t0.Add(time.Duration(m)*time.Minute), conds)
	}
	drain(srv.alerts)
	open, _ := srv.alerts.snapshot()
	if len(open) != 1 || open[0].Affected != "Mail, Webmail" || len(ch.got) != 1 || !strings.Contains(ch.got[0].Detail, "Also affected: Mail, Webmail") {
		t.Fatalf("open %+v sent %+v", open, ch.got)
	}
}

func TestHostWarningIsNotACause(t *testing.T) {
	srv := groupServer(t)
	disk := Condition{Key: "disk/node1//", Severity: "warning", Title: "node1 / is 85% full", Host: "node1"}
	nginx := Condition{Key: "unit/node2/nginx.service", Severity: "critical", Title: "nginx on node2: failed", Host: "node2"}
	got := srv.groupConditions([]Condition{disk, nginx, siteSlow}, never)
	if keys(got) != "warning:disk/node1// critical:unit/node2/nginx.service" || got[1].Affected != "Websites" {
		t.Fatalf("got %s %+v", keys(got), got)
	}
}

// Without topology.yaml, grouping works from the service map: only once
// the learned edge is steady, never over an alert already sent, never
// over an edge someone rejected, and the incident says which edge did it.
func TestGroupingFromLearnedEdges(t *testing.T) {
	srv, _ := testServer(t, false)
	now := time.Now()
	srv.store.Put("node1", report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{"10.0.0.1/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:webmail.service", Proto: "tcp", Addr: "*", Port: 443}},
		Conns:      []report.Conn{{Owner: "unit:webmail.service", Dir: "out", Remote: "10.0.0.2", Port: 5432, Count: 2}},
	}}, now)
	srv.store.Put("node2", report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{"10.0.0.2/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "*", Port: 5432}},
	}}, now)
	web := Condition{Key: "unit/node1/webmail.service", Severity: "warning", Title: "webmail.service on node1: failed", Host: "node1"}
	pg := Condition{Key: "unit/node2/postgresql.service", Severity: "critical", Title: "postgresql.service on node2: failed", Host: "node2"}
	group := func(firing func(string) bool) []Condition {
		t.Helper()
		srv.maps.graph = nil
		return srv.groupConditions([]Condition{web, pg}, firing)
	}

	// Seen once: not believed yet.
	if got := group(never); len(got) != 2 {
		t.Fatalf("grouped on an edge only seen: %+v", got)
	}
	if got := group(never); got[1].Map != "/map/services?focus=svc:unit:postgresql" {
		t.Errorf("map link: %q", got[1].Map)
	}
	for i := range 3 {
		srv.recordEdgesOnce(now.Add(time.Duration(i-3) * time.Hour))
	}
	got := group(never)
	if len(got) != 1 || got[0].Key != pg.Key || got[0].Affected != "webmail" || len(got[0].Grouped) != 1 {
		t.Fatalf("steady, not grouped: %+v", got)
	}
	if w := got[0].Grouped[0]; w.From != "unit:webmail" || w.To != "unit:postgresql" || w.Port != 5432 ||
		!strings.Contains(got[0].Detail, "Grouped because webmail has talked to postgresql :5432 since") {
		t.Errorf("why: %+v, %q", w, got[0].Detail)
	}
	if got := group(func(k string) bool { return k == web.Key }); len(got) != 2 {
		t.Errorf("an alert already sent was grouped: %+v", got)
	}
	srv.maps.fixes = &MapFixes{Rejected: []mapgraph.EdgeRef{{From: "unit:webmail", To: "unit:postgresql"}}}
	if got := group(never); len(got) != 2 {
		t.Errorf("grouped on a rejected edge: %+v", got)
	}
}
