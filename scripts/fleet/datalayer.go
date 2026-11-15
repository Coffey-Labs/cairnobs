package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The demo's data layer (Data layer page, docs/formats.md): site 0's first
// three databases are a Patroni cluster, its load balancers run HAProxy in
// front of it, its NATS hosts a three-node cluster, and its caches a Garage
// cluster. Each Patroni member answers on its own loopback address
// (pgAddr), so obs-beacond can match a member to its API as it does over a
// real network; the status pages come in the hosts' reports, as an agent's
// fetches do.
// Problems on purpose: the leader is fleet-0031, not its usual fleet-0030
// (a failover nobody moved back), and fleet-0032 is 40 MB behind.
const patroniPort = 58420

var (
	pgMembers   = []int{30, 31, 32}
	pgLeader    = 31
	pgHome      = 30
	pgLagging   = 32
	natsHosts   = []int{45, 46, 47}
	garageHosts = []int{40, 41, 42}
	etcdHosts   = pgMembers
	haproxyLBs  = []int{1, 2}
)

// hasDataLayer: the fleet has every host the data layer names.
func hasDataLayer(hosts int) bool { return hosts > 47 }

// dataLayerYAML is the datalayer: section for obs-beacond.yaml.
func dataLayerYAML() string {
	names := func(ns []int) string {
		var s []string
		for _, n := range ns {
			s = append(s, hostName(n))
		}
		return strings.Join(s, ", ")
	}
	var urls []string
	for _, m := range pgMembers {
		urls = append(urls, fmt.Sprintf("http://%s:%d", pgAddr(m), patroniPort))
	}
	return fmt.Sprintf(`datalayer:
  patroni: [%s]
  sync_standby: true
  preferred_leader: %s
  probes:
    - {id: haproxy, kind: haproxy, url: "http://127.0.0.1:8404/stats;csv", from: [%s], exactly_one: [pg-primary], at_least_one: [pg-replicas]}
    - {id: nats, kind: nats, url: "http://127.0.0.1:8222/varz", from: [%s], peers: 2}
    - {id: garage, kind: garage, url: "http://127.0.0.1:3903/health", from: [%s]}
    - {id: etcd, kind: etcd, url: "http://127.0.0.1:2381/metrics", from: [%s], fsync_warn_ms: 500}
`, strings.Join(urls, ", "), hostName(pgHome), names(haproxyLBs), names(natsHosts), names(garageHosts), names(etcdHosts))
}

// pgAddr is member n's loopback address.
func pgAddr(n int) string { return fmt.Sprintf("127.0.84.%d", n) }

// servePatroni answers /cluster and /patroni for each member.
func servePatroni(ctx context.Context) {
	for _, me := range pgMembers {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", pgAddr(me), patroniPort))
		if err != nil {
			log.Fatal(err)
		}
		srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/cluster":
				var ms []string
				for _, m := range pgMembers {
					role, state, lag := "replica", "streaming", `,"lag":0`
					switch m {
					case pgLeader:
						role, state, lag = "leader", "running", ""
					case pgHome:
						role = "sync_standby"
					case pgLagging:
						lag = `,"lag":41943040`
					}
					ms = append(ms, fmt.Sprintf(`{"name":%q,"role":%q,"state":%q,"host":%q,"port":5432,"timeline":7%s}`, hostName(m), role, state, pgAddr(m), lag))
				}
				fmt.Fprintf(w, `{"members":[%s],"scope":"shop-pg"}`, strings.Join(ms, ","))
			case "/patroni":
				role := "replica"
				if me == pgLeader {
					role = "primary"
				}
				fmt.Fprintf(w, `{"state":"running","role":%q,"dcs_last_seen":%d,"timeline":7}`, role, time.Now().Unix()-3)
			default:
				http.NotFound(w, r)
			}
		})}
		go func() { _ = srv.Serve(ln) }()
		go func() { <-ctx.Done(); _ = srv.Close() }()
	}
	log.Printf("fleet: Patroni cluster shop-pg on %s-%d port %d", pgAddr(pgMembers[0]), pgMembers[len(pgMembers)-1], patroniPort)
}

// dataFetches are the status pages host n reads, if any (site 0 only).
func dataFetches(n int, now time.Time) []report.Fetch {
	if n >= siteSize {
		return nil
	}
	at := now.Add(-2 * time.Second).UTC()
	var out []report.Fetch
	for _, lb := range haproxyLBs {
		if n != lb {
			continue
		}
		var b strings.Builder
		b.WriteString("# pxname,svname,qcur,status,weight\nstats,FRONTEND,,OPEN,\n")
		backend := func(name string, up func(m int) bool, servers []int) {
			fmt.Fprintf(&b, "%s,FRONTEND,,OPEN,\n", name)
			for _, m := range servers {
				st := "DOWN"
				if up(m) {
					st = "UP"
				}
				fmt.Fprintf(&b, "%s,%s,0,%s,1\n", name, hostName(m), st)
			}
			fmt.Fprintf(&b, "%s,BACKEND,0,UP,1\n", name)
		}
		backend("pg-primary", func(m int) bool { return m == pgLeader }, pgMembers)
		backend("pg-replicas", func(m int) bool { return m != pgLeader }, pgMembers)
		var apps []int
		for p := 3; p < 13; p++ {
			apps = append(apps, p)
		}
		backend("shop-web", func(int) bool { return true }, apps)
		out = append(out, report.Fetch{ID: "haproxy", At: at, Status: 200, Body: b.String()})
	}
	for k, m := range natsHosts {
		if n == m {
			out = append(out, report.Fetch{ID: "nats", At: at, Status: 200, Body: fmt.Sprintf(
				`{"server_name":%q,"version":"2.11.17","connections":%d,"routes":2,"remotes":2,"slow_consumers":0,"in_msgs":%d,"out_msgs":%d,"uptime":"21d4h"}`,
				hostName(m), 40+k*3, 1_900_000+k*1000, 2_400_000+k*1000)})
		}
	}
	for _, m := range garageHosts {
		if n == m {
			out = append(out, report.Fetch{ID: "garage", At: at, Status: 200, Body: "Garage is fully operational\nConsult the full health check API endpoint at /v1/health for more details\n"})
		}
	}
	for k, m := range etcdHosts {
		if n == m {
			// About 20 WAL syncs a second, most well under 16 ms.
			c := now.Unix()/5 + int64(k)*1000
			leader := 0
			if m == pgLeader {
				leader = 1
			}
			out = append(out, report.Fetch{ID: "etcd", At: at, Status: 200, Body: fmt.Sprintf(`etcd_server_has_leader 1
etcd_server_is_leader %d
etcd_server_leader_changes_seen_total 3
etcd_mvcc_db_total_size_in_bytes %g
etcd_server_quota_backend_bytes 2.147483648e+09
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.016"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.064"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="0.512"} %d
etcd_disk_wal_fsync_duration_seconds_bucket{le="+Inf"} %d
`, leader, 3.1e7+float64(k)*1e5, 90*c, 998*c/10, 100*c, 100*c)})
		}
	}
	return out
}
