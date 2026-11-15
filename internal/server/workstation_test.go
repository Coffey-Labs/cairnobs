package server

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// wsServer has three servers and one workstation, "desk", with the checks
// from checksYAML plus one that names desk.
func wsServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "checks.yaml")
	extra := `  - id: ping-node3-desk
    name: node3 over wg-desk
    ping: 10.31.0.1
    from: [desk]
    severity: warning
`
	_ = os.WriteFile(p, []byte(checksYAML+extra), 0o644)
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Checks: p,
		DataLayer: &DataLayerConfig{Probes: []ProbeConfig{{ID: "nats", Kind: "nats", URL: "http://127.0.0.1:8222/varz", Peers: 2}}}}
	for _, h := range []string{"node1", "node2", "node3", "desk"} {
		cfg.Agents = append(cfg.Agents, AgentConfig{Name: h, TokenSHA256: HashToken(NewAgentToken()), Workstation: h == "desk"})
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestWorkstationGetsOnlyWorkThatNamesIt(t *testing.T) {
	srv := wsServer(t)
	ids := func(agent string) string {
		w, _ := srv.workFor(agent)
		var out []string
		for _, c := range w.Checks {
			out = append(out, c.ID)
		}
		for _, f := range w.Fetches {
			out = append(out, "fetch:"+f.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("desk"); got != "ping-node3-desk" {
		t.Fatalf("desk's work: %q", got)
	}
	if got := ids("node1"); got != "site,mx,fetch:nats" {
		t.Fatalf("node1's work: %q", got)
	}

	// A check with no `from` is judged over the servers only.
	now := time.Now()
	srv.store.Put("node1", results(now, report.CheckResult{ID: "site", OK: true}), now)
	views, _ := srv.judgeChecks(now)
	if v := viewOf(t, views, "site"); slices.Contains(v.From, "desk") || len(v.From) != 3 {
		t.Fatalf("site runs from %v", v.From)
	}

	// And the Data layer page doesn't wait for desk's NATS reading.
	srv.store.Put("desk", report.Report{V: report.Version}, now)
	view, _ := srv.dataLayerView(now)
	for _, p := range view["probes"].([]ProbeView) {
		if p.Host == "desk" {
			t.Fatalf("probe row for desk: %+v", p)
		}
	}
}

func TestWorkstationNeverPages(t *testing.T) {
	srv := wsServer(t)
	srv.started = time.Now().Add(-10 * time.Minute)
	now := time.Now()
	const gb = 1 << 30

	// Never reported since obs-beacond started: silent, but only a warning.
	hosts, conds := srv.view(now)
	for _, c := range conds {
		if c.Host == "desk" && c.Severity != "warning" {
			t.Fatalf("desk before its first report: %+v", c)
		}
		if c.Host == "node1" && c.Key == "silent/node1" && c.Severity != "critical" {
			t.Fatalf("a server's silence must stay critical: %+v", c)
		}
	}
	for _, h := range hosts {
		if h.Name == "desk" && (h.Status != "warning" || !h.Workstation) {
			t.Fatalf("desk waiting: %+v", h)
		}
	}

	// A full disk and a stray container: still warning at most, and the
	// container isn't judged or counted.
	srv.store.Put("desk", report.Report{V: report.Version,
		Disks:      []report.Disk{{Mount: "/", Size: 100 * gb, Used: 95 * gb, Avail: 5 * gb}},
		Containers: &report.Containers{List: []report.Container{{ID: "a", Name: "ihasmail", Image: "ihasmail:dev", State: "exited"}}}}, now)
	hosts, conds = srv.view(now)
	var keys []string
	for _, c := range conds {
		if c.Host != "desk" {
			continue
		}
		keys = append(keys, c.Severity+":"+c.Key)
	}
	if got, want := strings.Join(keys, " "), "warning:disk/desk//"; got != want {
		t.Fatalf("desk's conditions\n got  %s\n want %s", got, want)
	}
	for _, h := range hosts {
		if h.Name == "desk" && (len(h.rows) != 0 || h.Containers.Total != 0) {
			t.Fatalf("desk's containers were judged: %+v %+v", h.rows, h.Containers)
		}
	}

	// Gone quiet after reporting: a warning, not a page.
	later := now.Add(5 * time.Minute)
	_, conds = srv.view(later)
	found := false
	for _, c := range conds {
		if c.Key == "silent/desk" {
			found = true
			if c.Severity != "warning" {
				t.Fatalf("desk silent: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("desk's silence wasn't noticed")
	}
}

// A workstation's backup scan is judged like a server's, but a failing
// disk there is a warning, not a page.
func TestWorkstationDiskHealthNeverPages(t *testing.T) {
	srv := wsServer(t)
	p := filepath.Join(t.TempDir(), "backups.yaml")
	_ = os.WriteFile(p, []byte("hosts:\n  node1: {}\n  desk: {}\n"), 0o600)
	srv.backups.path = p
	now := time.Now()
	failing := &report.Backups{At: now, SMART: []report.DiskHealth{{Disk: "sda", OK: false}, {Disk: "nvme0n1", OK: true}}}
	srv.store.Put("desk", report.Report{Backups: failing}, now)
	srv.store.Put("node1", report.Report{Backups: failing}, now)
	got := keys(srv.backupConditions(now))
	if got != "warning:backup/desk/smart/sda critical:backup/node1/smart/sda" {
		t.Errorf("got %q", got)
	}
}
