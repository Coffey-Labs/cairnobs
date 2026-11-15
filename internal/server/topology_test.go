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
    - {name: host1, roles: "mail · db"}
    - {name: host2}
    - {name: host3}
services:
  - {id: site, name: Websites, tier: public, needs: [host1], checks: [site], containers: ["*/example-org"]}
  - {id: mail, name: Mail, tier: apps, needs: [db], checks: [mx], units: ["*/inbuxa"]}
  - {id: db, name: PostgreSQL, tier: data, needs: [host1, host2], units: ["host1/patroni"]}
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
	_ = os.WriteFile(exp, []byte("hosts:\n  host1:\n    running: [example-org]\n    units: [inbuxa, patroni]\n  host2:\n    units: [inbuxa]\n"), 0o644)
	srv.cfg.Expected = exp
	return srv
}

func TestServiceStates(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	r := func(id string, ok bool, ms float64) report.CheckResult {
		return report.CheckResult{ID: id, At: now, OK: ok, Ms: ms, Error: map[bool]string{false: "no reply"}[ok]}
	}
	srv.store.Put("host1", report.Report{V: report.Version,
		Units:      []report.Unit{{Name: "inbuxa.service", Load: "loaded", Active: "active", Sub: "running"}, {Name: "patroni.service", Load: "loaded", Active: "failed", Sub: "failed"}},
		Containers: &report.Containers{List: []report.Container{{ID: "a", Name: "example-org", State: "running", Status: "Up 2 hours"}}},
		Checks:     []report.CheckResult{r("site", true, 120), r("mx", true, 50)}}, now)
	srv.store.Put("host2", report.Report{V: report.Version,
		Units:  []report.Unit{{Name: "inbuxa.service", Load: "loaded", Active: "active", Sub: "running"}},
		Checks: []report.CheckResult{r("site", true, 90), r("mx", true, 40)}}, now)
	srv.store.Put("host3", report.Report{V: report.Version,
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
		"site": "good/OK/check:example.org@,container:example-org@host1",
		"mail": "good/OK/check:mail.example.org SMTP@,unit:inbuxa.service@host1,unit:inbuxa.service@host2",
		"db":   "critical/1 problem/unit:patroni.service@host1",
	}
	if len(got) != len(want) {
		t.Errorf("services %v", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s:\n got  %s\n want %s", id, got[id], w)
		}
	}
	if c := srv.mapCorrections(); c.SiteName != "Datacenter · Rack A" || c.Roles["host1"] != "mail · db" || len(c.SiteHosts) != 3 {
		t.Errorf("corrections %+v", c)
	}
}

func TestBadTopology(t *testing.T) {
	agents := []AgentConfig{{Name: "host1"}, {Name: "bench", Workstation: true}}
	p := filepath.Join(t.TempDir(), "t.yaml")
	for _, bad := range []string{
		"services:\n  - {id: a, tier: cloud}\n",
		"services:\n  - {id: a, tier: public, needs: [nope]}\n",
		"services:\n  - {id: host1, tier: public}\n",
		"services:\n  - {id: a, tier: public, units: [patroni]}\n",
		"services:\n  - {id: a, tier: public, units: [host9/patroni]}\n",
		"network:\n  hosts: [{name: host7}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: host7}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: host1}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: bench}, {name: bench}]\n",
		// What only the hand-drawn maps read (gone 2026-11-14).
		"network:\n  hosts: [{name: host1, mesh_ip: 10.0.0.1}]\n",
		"network:\n  mail: [{name: mx.example.org, host: host1, check: mx}]\n",
		"network:\n  remote: [{name: bench, via: host1}]\n",
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
	_ = os.WriteFile(exp, []byte("hosts:\n  host1:\n    running: [example-org]\n    stopped:\n      - {match: example-org-old, note: rollback copy}\n"), 0o644)
	srv.cfg.Expected = exp
	now := time.Now()
	srv.store.Put("host1", report.Report{V: report.Version, Containers: &report.Containers{List: []report.Container{
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
