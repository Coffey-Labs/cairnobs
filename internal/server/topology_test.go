package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/report"
)

const topologyYAML = `network:
  site: Datacenter · Rack A
  hosts:
    - {name: host1, mesh_check: ping-host1}
    - {name: host2}
    - {name: host3}
  mail:
    - {name: mail.example.org, host: host1, check: mx}
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

func TestTopologyView(t *testing.T) {
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
		Checks: []report.CheckResult{r("site", true, 90), r("mx", true, 40), r("ping-host1", true, 0.6)}}, now)
	srv.store.Put("host3", report.Report{V: report.Version,
		Checks: []report.CheckResult{r("site", true, 80), r("mx", true, 45), r("ping-host1", false, 0)}}, now)

	v := srv.topologyView(now)
	b, _ := json.Marshal(v)
	var out struct {
		Tiers    []string `json:"tiers"`
		Services []struct {
			ID, Status, Word string
			Signals          []Signal
		} `json:"services"`
		Network struct {
			Links []struct {
				Hosts  []string `json:"hosts"`
				Ms     *float64 `json:"ms"`
				Status string   `json:"status"`
			} `json:"links"`
			Paths []PathView `json:"paths"`
		} `json:"network"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range out.Services {
		got[s.ID] = s.Status + "/" + s.Word + "/" + func() string {
			var k []string
			for _, sg := range s.Signals {
				k = append(k, sg.Kind+":"+sg.Name+"@"+sg.Host)
			}
			return strings.Join(k, ",")
		}()
	}
	for id, want := range map[string]string{
		"site":  "good/OK/check:example.org@,container:example-org@host1",
		"mail":  "good/OK/check:mail.example.org SMTP@,unit:inbuxa.service@host1,unit:inbuxa.service@host2",
		"db":    "critical/1 problem/unit:patroni.service@host1",
		"host3": "good/Up/host:host3@host3",
	} {
		if got[id] != want {
			t.Errorf("%s:\n got  %s\n want %s", id, got[id], want)
		}
	}
	if strings.Join(out.Tiers, ",") != "edge,public,apps,data,hosts" {
		t.Errorf("tiers %v", out.Tiers)
	}
	// host1–host2: host2 reaches host1 (0.6 ms); host1 doesn't ping host2 (no mesh_check): one direction, up.
	// host1–host3: host3 can't reach host1: failing one way.
	links := map[string]string{}
	for _, l := range out.Network.Links {
		ms := "-"
		if l.Ms != nil {
			ms = strings.TrimRight(strings.TrimRight(json.Number(func() string { b, _ := json.Marshal(*l.Ms); return string(b) }()).String(), "0"), ".")
		}
		links[strings.Join(l.Hosts, "-")] = l.Status + " " + ms
	}
	if links["host1-host2"] != "good 0.6" || links["host1-host3"] != "warning -" || links["host2-host3"] != "waiting -" {
		t.Errorf("links %v", links)
	}
	if len(out.Network.Paths) != 1 || out.Network.Paths[0].Kind != "mail" || out.Network.Paths[0].Status != "good" {
		t.Errorf("paths %+v", out.Network.Paths)
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
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: host7, via: host1}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: host1, via: host1}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: bench, via: host9}]\n",
		"network:\n  hosts: [{name: host1}]\n  remote: [{name: bench, via: host1}, {name: bench, via: host1}]\n",
	} {
		_ = os.WriteFile(p, []byte(bad), 0o644)
		if _, err := LoadTopology(p, agents); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func TestTopologyWebPathsFromCloudflare(t *testing.T) {
	srv := topoServer(t)
	tf := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tf, []byte("x"), 0o600)
	w, err := cloudflare.New(cloudflare.Config{TokenFile: tf})
	if err != nil {
		t.Fatal(err)
	}
	w.Set(cloudflare.Snapshot{Checked: time.Now(), Pools: []cloudflare.Pool{
		{Name: "web", Enabled: true, Healthy: true, Origins: []cloudflare.Origin{{Name: "host1", Enabled: true, Healthy: true}, {Name: "host2", Enabled: true, Healthy: false}}},
		{Name: "mail-ui", Enabled: true, Healthy: true, Origins: []cloudflare.Origin{{Name: "host1", Enabled: true, Healthy: true}, {Name: "host3", Enabled: false}}},
	}})
	srv.cf = w
	b, _ := json.Marshal(srv.topologyView(time.Now()))
	var out struct {
		Network struct{ Paths []PathView } `json:"network"`
	}
	_ = json.Unmarshal(b, &out)
	got := []string{}
	for _, p := range out.Network.Paths {
		if p.Kind == "public" {
			got = append(got, p.Host+"="+p.Status)
		}
	}
	if strings.Join(got, " ") != "host1=good host2=warning" {
		t.Fatalf("web paths: %v", got)
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
	b, _ := json.Marshal(srv.topologyView(now))
	if !strings.Contains(string(b), `"id":"site","name":"Websites","tier":"public","needs":["host1"],"status":"good","word":"OK"`) {
		t.Fatalf("site: %s", b)
	}
}

func TestServiceMapNamesTheCause(t *testing.T) {
	srv := topoServer(t)
	now := time.Now()
	fail := func(id string) report.CheckResult { return report.CheckResult{ID: id, At: now, Error: "refused"} }
	srv.store.Put("host1", report.Report{V: report.Version, Units: []report.Unit{{Name: "patroni.service", Load: "loaded", Active: "failed", Sub: "failed"}}, Checks: []report.CheckResult{fail("mx")}}, now)
	srv.store.Put("host2", report.Report{V: report.Version, Checks: []report.CheckResult{fail("mx")}}, now)
	b, _ := json.Marshal(srv.topologyView(now))
	if !strings.Contains(string(b), `"id":"mail","name":"Mail","tier":"apps","needs":["db"],"status":"critical","word":"1 problem","sub":"1 check · 2 units","cause":"PostgreSQL"`) {
		t.Fatalf("%s", b)
	}
}

// A remote host is drawn on its own, with its link judged from its reports
// and its ping to the far end.
func TestTopologyRemoteHost(t *testing.T) {
	dir := t.TempDir()
	checks := filepath.Join(dir, "checks.yaml")
	_ = os.WriteFile(checks, []byte("checks:\n  - {id: ping-host3, name: host3 over wg-bench, ping: 10.78.0.1, from: [bench], severity: warning}\n"), 0o644)
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Checks: checks}
	for _, h := range []string{"host1", "host3", "bench"} {
		cfg.Agents = append(cfg.Agents, AgentConfig{Name: h, TokenSHA256: HashToken(NewAgentToken()), Workstation: h == "bench"})
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "topology.yaml")
	_ = os.WriteFile(p, []byte("network:\n  hosts: [{name: host1}, {name: host3}]\n  remote:\n    - {name: bench, via: host3, link: wg-bench, link_check: ping-host3, mesh_ip: 10.78.0.2, roles: workstation}\n"), 0o644)
	if srv.topology.cur, err = LoadTopology(p, cfg.Agents); err != nil {
		t.Fatal(err)
	}
	type remote struct {
		Name        string
		Workstation bool
		Link        struct {
			Name, Via, Status, Word string
			Ms                      *float64
		}
	}
	look := func(now time.Time) remote {
		t.Helper()
		b, _ := json.Marshal(srv.topologyView(now))
		var out struct {
			Services []struct{ ID string } `json:"services"`
			Network  struct {
				Hosts  []struct{ Name string } `json:"hosts"`
				Remote []remote                `json:"remote"`
				Links  []LinkView              `json:"links"`
			} `json:"network"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Network.Hosts) != 2 || len(out.Network.Links) != 1 || len(out.Network.Remote) != 1 {
			t.Fatalf("bench must be neither a site host nor in the mesh: %s", b)
		}
		for _, s := range out.Services {
			if s.ID == "bench" {
				t.Fatalf("bench is on the service map")
			}
		}
		return out.Network.Remote[0]
	}

	now := time.Now()
	if r := look(now); r.Link.Status != "waiting" || r.Link.Via != "host3" || r.Link.Name != "wg-bench" || !r.Workstation {
		t.Errorf("before any report: %+v", r)
	}
	srv.store.Put("bench", results(now, report.CheckResult{ID: "ping-host3", OK: true, Ms: 171}), now)
	if r := look(now); r.Link.Status != "good" || r.Link.Ms == nil || *r.Link.Ms != 171 {
		t.Errorf("reporting, ping answers: %+v", r)
	}
	srv.store.Put("bench", results(now, report.CheckResult{ID: "ping-host3", OK: false, Error: "no reply"}), now)
	if r := look(now); r.Link.Status != "warning" || r.Link.Ms != nil {
		t.Errorf("reporting, ping fails: %+v", r)
	}
	// Silent: a workstation's silence is a warning, never critical.
	if r := look(now.Add(5 * time.Minute)); r.Link.Status != "warning" || r.Link.Word != "No reports coming over it" {
		t.Errorf("silent: %+v", r)
	}
}
