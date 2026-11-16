package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

const topologyYAML = `network:
  site: Datacenter · Rack A
  hosts:
    - {name: node1, roles: "mail · db"}
    - {name: node2}
    - {name: node3}
services:
  - {id: site, name: Websites, tier: public, needs: [node1], checks: [site], containers: ["*/example-org"]}
  - {id: mail, name: Mail, tier: apps, needs: [db], checks: [mx], units: ["*/inbuxa"]}
  - {id: db, name: PostgreSQL, tier: data, needs: [node1, node2], units: ["node1/patroni"]}
`

func topoServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := checkServer(t)
	p := filepath.Join(t.TempDir(), "topology.yaml")
	_ = os.WriteFile(p, []byte(topologyYAML), 0o644)
	topo, err := LoadTopology(p, srv.cfg.Agents)
	if err != nil {
		t.Fatal(err)
	}
	srv.topology.cur = topo
	exp := filepath.Join(t.TempDir(), "expected.yaml")
	_ = os.WriteFile(exp, []byte("hosts:\n  node1:\n    running: [example-org]\n    units: [inbuxa, patroni]\n  node2:\n    units: [inbuxa]\n"), 0o644)
	srv.cfg.Expected = exp
	return srv
}

func TestServiceStates(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	r := func(id string, ok bool, ms float64) report.CheckResult {
		return report.CheckResult{ID: id, At: now, OK: ok, Ms: ms, Error: map[bool]string{false: "no reply"}[ok]}
	}
	srv.store.Put("node1", report.Report{V: report.Version,
		Units:      []report.Unit{{Name: "inbuxa.service", Load: "loaded", Active: "active", Sub: "running"}, {Name: "patroni.service", Load: "loaded", Active: "failed", Sub: "failed"}},
		Containers: &report.Containers{List: []report.Container{{ID: "a", Name: "example-org", State: "running", Status: "Up 2 hours"}}},
		Checks:     []report.CheckResult{r("site", true, 120), r("mx", true, 50)}}, now)
	srv.store.Put("node2", report.Report{V: report.Version,
		Units:  []report.Unit{{Name: "inbuxa.service", Load: "loaded", Active: "active", Sub: "running"}},
		Checks: []report.CheckResult{r("site", true, 90), r("mx", true, 40)}}, now)
	srv.store.Put("node3", report.Report{V: report.Version,
		Checks: []report.CheckResult{r("site", true, 80), r("mx", true, 45)}}, now)

	svcs, ok := srv.serviceStates(now)
	if !ok {
		t.Fatal("no topology")
	}
	got := map[string]string{}
	for _, s := range svcs {
		var k []string
		for _, sg := range s.Signals {
			k = append(k, sg.Kind+":"+sg.Name+"@"+sg.Host)
		}
		got[s.ID] = s.Status + "/" + s.Word + "/" + strings.Join(k, ",")
	}
	want := map[string]string{
		"site": "good/OK/check:example.org@,container:example-org@node1",
		"mail": "good/OK/check:mail.example.org SMTP@,unit:inbuxa.service@node1,unit:inbuxa.service@node2",
		"db":   "critical/1 problem/unit:patroni.service@node1",
	}
	if len(got) != len(want) {
		t.Errorf("services %v", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s:\n got  %s\n want %s", id, got[id], w)
		}
	}
	if c := srv.mapCorrections(); c.SiteName != "Datacenter · Rack A" || c.Roles["node1"] != "mail · db" || len(c.SiteHosts) != 3 {
		t.Errorf("corrections %+v", c)
	}
}

func TestBadTopology(t *testing.T) {
	agents := []AgentConfig{{Name: "node1"}, {Name: "desk", Workstation: true}}
	p := filepath.Join(t.TempDir(), "t.yaml")
	for _, bad := range []string{
		"services:\n  - {id: a, tier: cloud}\n",
		"services:\n  - {id: a, tier: public, needs: [nope]}\n",
		"services:\n  - {id: node1, tier: public}\n",
		"services:\n  - {id: a, tier: public, units: [patroni]}\n",
		"services:\n  - {id: a, tier: public, units: [node9/patroni]}\n",
		"network:\n  hosts: [{name: node7}]\n",
		"network:\n  hosts: [{name: node1}]\n  remote: [{name: node7}]\n",
		"network:\n  hosts: [{name: node1}]\n  remote: [{name: node1}]\n",
		"network:\n  hosts: [{name: node1}]\n  remote: [{name: desk}, {name: desk}]\n",
		// What only the hand-drawn maps read (gone 2026-11-14).
		"network:\n  hosts: [{name: node1, mesh_ip: 10.0.0.1}]\n",
		"network:\n  mail: [{name: mx.example.org, host: node1, check: mx}]\n",
		"network:\n  remote: [{name: desk, via: node1}]\n",
		"services:\n  - {id: a, tier: public, sub: two words}\n",
	} {
		_ = os.WriteFile(p, []byte(bad), 0o644)
		if _, err := LoadTopology(p, agents); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func TestStoppedOnPurposeDoesNotGreyAService(t *testing.T) {
	srv := topoServer(t)
	exp := filepath.Join(t.TempDir(), "expected.yaml")
	_ = os.WriteFile(exp, []byte("hosts:\n  node1:\n    running: [example-org]\n    stopped:\n      - {match: example-org-old, note: rollback copy}\n"), 0o644)
	srv.cfg.Expected = exp
	now := time.Now()
	srv.store.Put("node1", report.Report{V: report.Version, Containers: &report.Containers{List: []report.Container{
		{ID: "a", Name: "example-org", State: "running", Status: "Up 2 hours"},
		{ID: "b", Name: "example-org-old", State: "exited", Status: "Exited (0) 2 days ago"},
	}}}, now)
	topo := srv.currentTopology()
	topo.Services[0].Containers = []string{"*/example-org*"}
	topo.Services[0].Checks = nil
	svcs, _ := srv.serviceStates(now)
	if len(svcs) == 0 || svcs[0].ID != "site" || svcs[0].Status != "good" || svcs[0].Word != "OK" {
		t.Fatalf("site: %+v", svcs)
	}
}

// A declared service judged only by its checks (no parts found) takes
// their state on the service map, as the hand-drawn map gave it.
func TestServiceMapJudgesByChecks(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	look := func() string {
		t.Helper()
		v, ok := srv.servicesView("", 3, now)
		if !ok {
			t.Fatal("no view")
		}
		for _, b := range v.Boxes {
			if b.ID == "svc:mail" {
				return b.Status + "/" + b.Sub
			}
		}
		t.Fatalf("no mail box in %+v", v.Boxes)
		return ""
	}
	for _, h := range []string{"node1", "node2", "node3"} {
		srv.store.Put(h, report.Report{V: report.Version, Checks: []report.CheckResult{{ID: "mx", At: now, OK: true, Ms: 40}}}, now)
	}
	if got := look(); got != "good/OK" {
		t.Errorf("checks pass: %s", got)
	}
	srv.store.Put("node2", report.Report{V: report.Version, Checks: []report.CheckResult{{ID: "mx", At: now, Error: "refused"}}}, now)
	if got := look(); !strings.HasPrefix(got, "critical/mail.example.org SMTP: ") && !strings.HasPrefix(got, "warning/mail.example.org SMTP: ") {
		t.Errorf("a check fails: %s", got)
	}
}

// A unit the topology claims that has failed colours its service on the
// map, though a failed unit no longer listens and so isn't one of its parts.
func TestServiceMapJudgesByUnits(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	look := func() string {
		t.Helper()
		v, ok := srv.servicesView("", 3, now)
		if !ok {
			t.Fatal("no view")
		}
		for _, b := range v.Boxes {
			if b.ID == "svc:db" {
				return b.Status + "/" + b.Sub
			}
		}
		t.Fatalf("no db box in %+v", v.Boxes)
		return ""
	}
	unit := func(active string) report.Report {
		return report.Report{V: report.Version, Units: []report.Unit{{Name: "patroni.service", Load: "loaded", Active: active, Sub: active, Since: now}}}
	}
	srv.store.Put("node1", unit("active"), now)
	if got := look(); got != "good/OK" {
		t.Errorf("patroni running: %s", got)
	}
	srv.store.Put("node1", unit("failed"), now)
	if got := look(); !strings.HasPrefix(got, "critical/patroni.service") {
		t.Errorf("patroni failed: %s", got)
	}
}

// One copy of a unit down while another runs is the service degraded,
// not down; every copy down is down (#52).
func TestServiceRedundantCopies(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	mail := func() ServiceView {
		t.Helper()
		svcs, ok := srv.serviceStates(now)
		if !ok {
			t.Fatal("no services")
		}
		for _, s := range svcs {
			if s.ID == "mail" {
				return s
			}
		}
		t.Fatal("no mail service")
		return ServiceView{}
	}
	unit := func(active string) report.Report {
		return report.Report{V: report.Version, Units: []report.Unit{{Name: "inbuxa.service", Load: "loaded", Active: active, Sub: active, Since: now}}}
	}
	srv.store.Put("node1", unit("failed"), now)
	srv.store.Put("node2", unit("active"), now)
	if m := mail(); m.Status != "warning" || m.Word != "1 of 2 inbuxa.service down" {
		t.Errorf("one of two down: %s %q", m.Status, m.Word)
	}
	srv.store.Put("node2", unit("failed"), now)
	if m := mail(); m.Status != "critical" {
		t.Errorf("both down: %s %q", m.Status, m.Word)
	}
}
