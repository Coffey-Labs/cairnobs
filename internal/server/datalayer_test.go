package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// HAProxy's CSV as node1 answered it (trimmed to the columns that matter).
const haproxyCSV = `# pxname,svname,qcur,status,weight
stats,FRONTEND,,OPEN,
pg-primary,FRONTEND,,OPEN,
pg-primary,node1,0,DOWN,1
pg-primary,node2,0,UP,1
pg-primary,node3,0,DOWN,1
pg-primary,BACKEND,0,UP,1
pg-replicas,FRONTEND,,OPEN,
pg-replicas,node1,0,UP,1
pg-replicas,node2,0,DOWN,1
pg-replicas,node3,0,UP,1
pg-replicas,BACKEND,0,UP,2
s3,FRONTEND,,OPEN,
s3,node1,0,UP,1
s3,node2,0,UP,1
s3,node3,0,%s,1
s3,BACKEND,0,UP,3
`

// fakePatroni answers /cluster and /patroni like a member does.
type fakePatroni struct {
	mu      sync.Mutex
	members string
	dcs     int64
}

func (f *fakePatroni) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/cluster":
			fmt.Fprintf(w, `{"members": [%s], "scope": "inbuxa"}`, f.members)
		case "/patroni":
			fmt.Fprintf(w, `{"state": "running", "role": "replica", "dcs_last_seen": %d}`, f.dcs)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func dataServer(t *testing.T, patroniURL string) *Server {
	t.Helper()
	srv, _ := checkServer(t)
	srv.cfg.DataLayer = &DataLayerConfig{Patroni: []string{patroniURL}, SyncStandby: true, Probes: []ProbeConfig{
		{ID: "haproxy", Kind: "haproxy", URL: "http://127.0.0.1:8404/stats;csv", ExactlyOne: []string{"pg-primary"}, AtLeastOne: []string{"pg-replicas"}},
		{ID: "nats", Kind: "nats", URL: "http://127.0.0.1:8222/varz", Peers: 2},
		{ID: "garage", Kind: "garage", URL: "http://127.0.0.1:3903/health"},
	}}
	if err := srv.cfg.DataLayer.check(map[string]bool{"node1": true, "node2": true, "node3": true}); err != nil {
		t.Fatal(err)
	}
	return srv
}

func put(srv *Server, host string, now time.Time, fetches ...report.Fetch) {
	for i := range fetches {
		fetches[i].At = now
	}
	srv.store.Put(host, report.Report{V: report.Version, Fetches: fetches}, now)
}

func natsBody(remotes, slow int) string {
	return fmt.Sprintf(`{"server_name":"x","version":"2.11.17","connections":1,"routes":8,"remotes":%d,"slow_consumers":%d,"in_msgs":5,"out_msgs":6,"uptime":"13d"}`, remotes, slow)
}

func TestDataLayerHealthy(t *testing.T) {
	now := time.Now()
	fp := &fakePatroni{dcs: now.Unix() - 5, members: `
		{"name":"node1","role":"replica","state":"streaming","host":"127.0.0.1","timeline":9,"lag":0},
		{"name":"node2","role":"leader","state":"running","host":"10.0.0.2","timeline":9},
		{"name":"node3","role":"sync_standby","state":"streaming","host":"10.0.0.3","timeline":9,"lag":"unknown"}`}
	ps := fp.server(t)
	srv := dataServer(t, ps.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go srv.pollPatroni(ctx)
	defer cancel()
	waitFor(t, func() bool { srv.patroni.mu.Lock(); defer srv.patroni.mu.Unlock(); return len(srv.patroni.reads) == 1 })

	for _, h := range []string{"node1", "node2", "node3"} {
		put(srv, h, now,
			report.Fetch{ID: "haproxy", Status: 200, Body: fmt.Sprintf(haproxyCSV, "UP")},
			report.Fetch{ID: "nats", Status: 200, Body: natsBody(2, 0)},
			report.Fetch{ID: "garage", Status: 200, Body: "Garage is fully operational\nConsult the full health check API endpoint"})
	}
	v, conds := srv.dataLayerView(now)
	if len(conds) != 0 {
		t.Fatalf("healthy cluster raised: %s", keys(conds))
	}
	srv.cfg.DataLayer.PreferredLeader = "node1"
	if _, c := srv.dataLayerView(now); keys(c) != "warning:dl/patroni/home" {
		t.Errorf("leader away from home: got %q", keys(c))
	}
	srv.cfg.DataLayer.PreferredLeader = "node2"
	b, _ := json.Marshal(v)
	for _, want := range []string{`"leader":"node2"`, `"lag_bytes":0`, `"dcs_age_s":5`, `"rule":"exactly one"`, `"says":"Garage is fully operational"`, `"peers":2`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("view lacks %s: %s", want, b)
		}
	}
}

func TestDataLayerTrouble(t *testing.T) {
	now := time.Now()
	lag := 40 << 20
	fp := &fakePatroni{dcs: now.Unix() - 300, members: fmt.Sprintf(`
		{"name":"node1","role":"replica","state":"streaming","host":"127.0.0.1","timeline":9,"lag":%d},
		{"name":"node3","role":"replica","state":"starting","host":"10.0.0.3","timeline":9}`, lag)}
	ps := fp.server(t)
	srv := dataServer(t, ps.URL)
	srv.cfg.DataLayer.Patroni = append(srv.cfg.DataLayer.Patroni, "http://127.0.0.1:1") // a second member that doesn't answer
	ctx, cancel := context.WithCancel(context.Background())
	go srv.pollPatroni(ctx)
	defer cancel()
	waitFor(t, func() bool { srv.patroni.mu.Lock(); defer srv.patroni.mu.Unlock(); return len(srv.patroni.reads) == 2 })

	put(srv, "node1", now,
		report.Fetch{ID: "haproxy", Status: 200, Body: fmt.Sprintf(haproxyCSV, "DOWN")},
		report.Fetch{ID: "nats", Status: 200, Body: natsBody(1, 0)},
		report.Fetch{ID: "garage", Status: 503, Body: "Garage is not operational: 1 partition has no quorum"})
	put(srv, "node2", now, report.Fetch{ID: "haproxy", Error: "dial tcp 127.0.0.1:8404: connect: connection refused"},
		report.Fetch{ID: "nats", Status: 200, Body: natsBody(0, 0)})
	_, conds := srv.dataLayerView(now)
	got := keys(conds)
	for _, want := range []string{
		"critical:dl/patroni/leader",      // nobody leads
		"warning:dl/patroni/member/node3", // starting
		"warning:dl/patroni/lag/node1",    // 40 MB behind
		"warning:dl/etcd/dcs/node1",       // 5 min without etcd
		"warning:dl/haproxy/s3/node1",     // one of three s3 servers down
		"warning:dl/nats/peers/node1",     // 1 of 2
		"critical:dl/garage/health/node1",
		"critical:dl/haproxy/down/node2", // status page not answering
		"critical:dl/nats/peers/node2",   // 0 of 2
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	// Slow consumers growing between readings warn.
	put(srv, "node3", now, report.Fetch{ID: "nats", Status: 200, Body: natsBody(2, 3)})
	srv.dataLayerView(now)
	put(srv, "node3", now, report.Fetch{ID: "nats", Status: 200, Body: natsBody(2, 7)})
	if _, conds := srv.dataLayerView(now); !strings.Contains(keys(conds), "warning:dl/nats/slow/node3") {
		t.Fatalf("slow consumers: %s", keys(conds))
	}
}

func TestPatroniUnreachableAndLeaderChange(t *testing.T) {
	now := time.Now()
	srv := dataServer(t, "http://127.0.0.1:1")
	srv.patroni.reads["http://127.0.0.1:1"] = patroniRead{at: now, err: fmt.Errorf("dial tcp 127.0.0.1:1: connect: connection refused")}
	if _, conds := srv.dataLayerView(now); !strings.Contains(keys(conds), "critical:dl/patroni/unreachable") {
		t.Fatalf("unreachable: %s", keys(conds))
	}
	// The leader moving goes in the audit log.
	srv.patroni.reads["http://127.0.0.1:1"] = patroniRead{at: now, members: []patroniMember{{Name: "node2", Role: "leader", State: "running"}}}
	srv.dataLayerView(now)
	srv.patroni.reads["http://127.0.0.1:1"] = patroniRead{at: now, members: []patroniMember{{Name: "node1", Role: "leader", State: "running"}}}
	srv.dataLayerView(now)
	if srv.patroni.lastLeader != "node1" {
		t.Fatalf("last leader %q", srv.patroni.lastLeader)
	}
}

func TestDataLayerConfigChecks(t *testing.T) {
	agents := map[string]bool{"node1": true}
	for _, bad := range []DataLayerConfig{
		{Probes: []ProbeConfig{{ID: "a", Kind: "redis", URL: "http://127.0.0.1/"}}},
		{Probes: []ProbeConfig{{ID: "a", Kind: "nats", URL: "http://example.com/varz"}}},
		{Probes: []ProbeConfig{{ID: "a", Kind: "nats", URL: "http://127.0.0.1/"}, {ID: "a", Kind: "nats", URL: "http://127.0.0.1/"}}},
		{Probes: []ProbeConfig{{ID: "a", Kind: "nats", URL: "http://127.0.0.1/", From: []string{"node9"}}}},
	} {
		if err := bad.check(agents); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// etcdMetrics is etcd's metrics page, already cut to the lines Cairn OBS keeps.
func etcdMetrics(hasLeader int, size float64, fsync [4]int) string {
	return fmt.Sprintf(`etcd_server_has_leader %d
etcd_server_is_leader 0
etcd_server_leader_changes_seen_total 2
etcd_mvcc_db_total_size_in_bytes %g
etcd_server_quota_backend_bytes 2.147483648e+09
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.016"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.064"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.512"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="+Inf"} %d
`, hasLeader, size, fsync[0], fsync[1], fsync[2], fsync[3])
}

func TestEtcdProbe(t *testing.T) {
	now := time.Now()
	srv := dataServer(t, "http://127.0.0.1:1")
	srv.cfg.DataLayer.Probes = append(srv.cfg.DataLayer.Probes, ProbeConfig{ID: "etcd", Kind: "etcd", URL: "http://127.0.0.1:2381/metrics", FsyncWarnMS: 500})
	read := func(host, body string) []Condition {
		put(srv, host, now, report.Fetch{ID: "etcd", Status: 200, Body: body})
		_, conds := srv.dataLayerView(now)
		var mine []Condition
		for _, c := range conds {
			if strings.HasPrefix(c.Key, "dl/etcd/") {
				mine = append(mine, c)
			}
		}
		return mine
	}
	// Healthy, on spinning disks: syncs up to 64 ms are fine under a 500 ms threshold.
	if c := read("node1", etcdMetrics(1, 1.1e6, [4]int{21, 40, 40, 40})); len(c) != 0 {
		t.Fatalf("first reading: %s", keys(c))
	}
	if c := read("node1", etcdMetrics(1, 1.1e6, [4]int{30, 80, 80, 80})); len(c) != 0 {
		t.Fatalf("64 ms syncs under a 500 ms threshold: %s", keys(c))
	}
	// Judged again before the next report, as the page and the evaluator
	// both do: the figure stays.
	for range 2 {
		v, _ := srv.dataLayerView(now)
		var got any
		for _, pv := range v["probes"].([]ProbeView) {
			if pv.Kind == "etcd" && pv.Host == "node1" {
				got = pv.Facts["fsync_p99_ms"]
			}
		}
		if got != 64.0 {
			t.Fatalf("p99 on a second look at the same report: %v, want 64", got)
		}
	}
	// Then syncs slower than 512 ms, no leader, and nearly full.
	c := read("node1", etcdMetrics(0, 1.8e9, [4]int{30, 80, 80, 120}))
	if got := keys(c); got != "critical:dl/etcd/leader/node1 warning:dl/etcd/quota/node1 warning:dl/etcd/fsync/node1" {
		t.Fatalf("trouble: %s", got)
	}
	if !strings.Contains(c[2].Detail, "took up to over 8 s") && !strings.Contains(c[2].Detail, "99% of the last 40 WAL syncs") {
		t.Fatalf("fsync detail: %s", c[2].Detail)
	}
	// The agent is asked to keep only the lines Cairn OBS reads.
	work, _ := srv.workFor("node1")
	for _, f := range work.Fetches {
		if f.ID == "etcd" && len(f.Keep) == 0 {
			t.Fatal("etcd fetch keeps everything")
		}
	}
}
