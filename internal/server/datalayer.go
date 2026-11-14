package server

// The data layer: PostgreSQL through Patroni, etcd as
// Patroni sees it, and HAProxy, NATS and Garage through the status pages
// each agent reads on its own host. obs-beacond does the reading of formats,
// so a change here never needs new agents.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/fetch"
	"github.com/cairnobs/cairnobs/internal/report"
)

// DataLayerConfig is the `datalayer` section of obs-beacond.yaml.
type DataLayerConfig struct {
	Patroni     []string `yaml:"patroni"`      // each member's API, e.g. http://10.0.0.1:8008 (obs-beacond asks them over the mesh)
	SyncStandby bool     `yaml:"sync_standby"` // there should always be one
	// Where the leader normally is; elsewhere means a failover happened and
	// someone should move it back (inbuxa-cluster switchover-home).
	PreferredLeader string        `yaml:"preferred_leader"`
	LagWarnMB       int           `yaml:"lag_warn_mb"` // a replica further behind than this warns; default 16
	DCSStale        time.Duration `yaml:"dcs_stale"`   // Patroni not hearing from etcd this long warns; default 60s
	Probes          []ProbeConfig `yaml:"probes"`
}

// ProbeConfig is a status page every agent (or those in From) reads on its host.
type ProbeConfig struct {
	ID   string   `yaml:"id"`
	Kind string   `yaml:"kind"` // haproxy, nats, garage or etcd (its metrics listener)
	URL  string   `yaml:"url"`
	From []string `yaml:"from"`

	// haproxy: backends where exactly one server, or at least one, should
	// be up (a database primary, its replicas); every other backend wants
	// all its servers up.
	ExactlyOne []string `yaml:"exactly_one"`
	AtLeastOne []string `yaml:"at_least_one"`
	// nats: how many other servers each one should be connected to.
	Peers int `yaml:"peers"`
	// etcd: a WAL fsync p99 above this warns (default 25 ms; spinning
	// disks need more). The share of the space quota that warns (75).
	FsyncWarnMS  int `yaml:"fsync_warn_ms"`
	QuotaWarnPct int `yaml:"quota_warn_pct"`
}

// etcdKeep are the only metrics lines obs-beacond reads: the agent drops the rest.
var etcdKeep = []string{"etcd_server_has_leader ", "etcd_server_is_leader ", "etcd_server_leader_changes_seen_total ",
	"etcd_mvcc_db_total_size_in_bytes ", "etcd_server_quota_backend_bytes ", "etcd_disk_wal_fsync_duration_seconds_bucket"}

// fetchSpec is what an agent is asked to read for a probe.
func (p ProbeConfig) fetchSpec() fetch.Spec {
	s := fetch.Spec{ID: p.ID, URL: p.URL}
	if p.Kind == "etcd" {
		s.Keep = etcdKeep
	}
	return s
}

func (c *DataLayerConfig) check(agents map[string]bool) error {
	if c.LagWarnMB == 0 {
		c.LagWarnMB = 16
	}
	if c.DCSStale == 0 {
		c.DCSStale = time.Minute
	}
	ids := map[string]bool{}
	for _, p := range c.Probes {
		if p.ID == "" || ids[p.ID] {
			return fmt.Errorf("datalayer: probe with a missing or repeated id %q", p.ID)
		}
		ids[p.ID] = true
		switch p.Kind {
		case "haproxy", "nats", "garage", "etcd":
		default:
			return fmt.Errorf("datalayer: probe %s: kind is haproxy, nats, garage or etcd", p.ID)
		}
		if err := (fetch.Spec{ID: p.ID, URL: p.URL}).Check(); err != nil {
			return fmt.Errorf("datalayer: %w", err)
		}
		for _, h := range p.From {
			if agents != nil && !agents[h] { // nil: names are checked later (New), against joined agents too
				return fmt.Errorf("datalayer: probe %s: no agent called %q", p.ID, h)
			}
		}
	}
	return nil
}

// --- Patroni, read by obs-beacond itself ---

type patroniMember struct {
	Name     string          `json:"name"`
	Role     string          `json:"role"`
	State    string          `json:"state"`
	Host     string          `json:"host"`
	Timeline int             `json:"timeline"`
	Lag      json.RawMessage `json:"lag"` // bytes, or "unknown"
}

type patroniNode struct {
	State       string `json:"state"`
	Role        string `json:"role"`
	DCSLastSeen int64  `json:"dcs_last_seen"`
}

type patroniRead struct {
	at      time.Time
	err     error
	members []patroniMember
	node    *patroniNode
}

type patroniWatch struct {
	mu         sync.Mutex
	reads      map[string]patroniRead // by member URL
	lastLeader string
}

// pollPatroni asks each member how the cluster is, every 15 seconds: on
// the evaluator (readings.go).
func (srv *Server) pollPatroni(ctx context.Context) {
	srv.poll(ctx, readingPatroni, patroniEvery, srv.patroniRound)
}

func (srv *Server) patroniRound(ctx context.Context) {
	urls := srv.cfg.DataLayer.Patroni
	hc := &http.Client{Timeout: 4 * time.Second}
	get := func(u string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 && resp.StatusCode != 503 { // 503 is Patroni saying "not the leader": still a valid body
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
	}
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := patroniRead{at: time.Now()}
			var cl struct {
				Members []patroniMember `json:"members"`
			}
			if r.err = get(strings.TrimRight(u, "/")+"/cluster", &cl); r.err == nil {
				r.members = cl.Members
				var n patroniNode
				if get(strings.TrimRight(u, "/")+"/patroni", &n) == nil {
					r.node = &n
				}
			}
			srv.patroni.mu.Lock()
			srv.patroni.reads[u] = r
			srv.patroni.mu.Unlock()
		}()
	}
	wg.Wait()
}

func lagBytes(raw json.RawMessage) (int64, bool) {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}

// --- the views the Data layer page shows ---

// PGMember is one PostgreSQL member as the page shows it.
type PGMember struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	State    string `json:"state"`
	Timeline int    `json:"timeline"`
	LagBytes *int64 `json:"lag_bytes,omitempty"`
	DCSAgeS  *int   `json:"dcs_age_s,omitempty"` // how long since this member's Patroni heard from etcd
	Status   string `json:"status"`
	Word     string `json:"word"`
}

// HABackend is one HAProxy backend on one host.
type HABackend struct {
	Name   string   `json:"name"`
	Up     []string `json:"up"`
	Down   []string `json:"down"`
	Rule   string   `json:"rule"` // all, exactly one, at least one
	Status string   `json:"status"`
	Word   string   `json:"word"`
}

// ProbeView is one status page from one host.
type ProbeView struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Host     string         `json:"host"`
	Status   string         `json:"status"`
	Word     string         `json:"word"`
	Backends []HABackend    `json:"backends,omitempty"` // haproxy
	Facts    map[string]any `json:"facts,omitempty"`    // nats, garage
}

// dataLayerView judges the data layer now.
func (srv *Server) dataLayerView(now time.Time) (map[string]any, []Condition) {
	dl := srv.cfg.DataLayer
	if dl == nil {
		return map[string]any{"configured": false}, nil
	}
	const href = "/data"
	var conds []Condition
	add := func(key, sev, title, detail, host string) {
		conds = append(conds, Condition{Key: "dl/" + key, Severity: sev, Title: title, Detail: detail, Host: host, Href: href})
	}

	// PostgreSQL: the first member that answered recently speaks for the cluster.
	pg := map[string]any{"configured": len(dl.Patroni) > 0}
	if len(dl.Patroni) > 0 {
		srv.patroni.mu.Lock()
		reads := map[string]patroniRead{}
		for k, v := range srv.patroni.reads {
			reads[k] = v
		}
		srv.patroni.mu.Unlock()
		var view []patroniMember
		var errs []string
		dcs := map[string]int{} // member name → seconds since it heard from etcd
		for _, u := range dl.Patroni {
			r, ok := reads[u]
			switch {
			case !ok:
			case r.err != nil || now.Sub(r.at) > time.Minute:
				if r.err != nil {
					errs = append(errs, u+": "+plainNetErr(r.err))
				}
			default:
				if view == nil {
					view = r.members
				}
				if r.node != nil && r.node.DCSLastSeen > 0 {
					pu, _ := url.Parse(u)
					for _, m := range r.members {
						if pu != nil && pu.Hostname() == m.Host {
							dcs[m.Name] = int(now.Unix() - r.node.DCSLastSeen)
						}
					}
				}
			}
		}
		members := []PGMember{}
		leader := ""
		sync := false
		for _, m := range view {
			pm := PGMember{Name: m.Name, Role: m.Role, State: m.State, Timeline: m.Timeline, Status: "good", Word: "OK"}
			if n, ok := lagBytes(m.Lag); ok {
				pm.LagBytes = &n
			}
			if a, ok := dcs[m.Name]; ok {
				pm.DCSAgeS = &a
			}
			switch m.Role {
			case "leader", "master", "standby_leader":
				leader = m.Name
			case "sync_standby":
				sync = true
			}
			if m.State != "running" && m.State != "streaming" {
				pm.Status, pm.Word = "warning", m.State
				add("patroni/member/"+m.Name, "warning", fmt.Sprintf("PostgreSQL on %s: %s", m.Name, m.State), "Patroni reports this member isn't running or streaming.", m.Name)
			}
			if pm.LagBytes != nil && *pm.LagBytes > int64(dl.LagWarnMB)<<20 {
				pm.Status, pm.Word = "warning", fmt.Sprintf("%d MB behind", *pm.LagBytes>>20)
				add("patroni/lag/"+m.Name, "warning", fmt.Sprintf("PostgreSQL replica %s is %d MB behind", m.Name, *pm.LagBytes>>20), fmt.Sprintf("Warns over %d MB.", dl.LagWarnMB), m.Name)
			}
			if pm.DCSAgeS != nil && time.Duration(*pm.DCSAgeS)*time.Second > dl.DCSStale {
				add("etcd/dcs/"+m.Name, "warning", fmt.Sprintf("Patroni on %s hasn't heard from etcd for %s", m.Name, human(time.Duration(*pm.DCSAgeS)*time.Second)),
					"etcd is Patroni's source of truth for who leads; if it stays out of reach, no failover can happen.", m.Name)
			}
			members = append(members, pm)
		}
		switch {
		case view == nil && len(reads) > 0:
			add("patroni/unreachable", "critical", "Can't reach PostgreSQL's cluster API", strings.Join(errs, "; ")+".", "")
		case view != nil && leader == "":
			add("patroni/leader", "critical", "PostgreSQL has no leader", "No member is leading: writes fail until one is elected.", "")
		case view != nil && dl.SyncStandby && !sync:
			add("patroni/sync", "warning", "PostgreSQL has no sync standby", "Commits aren't confirmed on a second member until one catches up.", "")
		}
		if leader != "" && dl.PreferredLeader != "" && leader != dl.PreferredLeader {
			add("patroni/home", "warning", fmt.Sprintf("PostgreSQL's leader is %s, not %s", leader, dl.PreferredLeader),
				"A failover happened. Once things are calm, inbuxa-cluster switchover-home moves it back.", "")
		}
		if view != nil && len(view) < len(dl.Patroni) {
			add("patroni/members", "warning", fmt.Sprintf("PostgreSQL has %d of %d members", len(view), len(dl.Patroni)), "A member has left the cluster.", "")
		}
		// A new leader is worth a line in the audit log: an unplanned
		// failover shows up there. The evaluator writes it, so it's there once.
		srv.patroni.mu.Lock()
		if leader != "" && srv.patroni.lastLeader != "" && leader != srv.patroni.lastLeader && srv.leading() {
			go srv.audit("obs-beacond", "saw PostgreSQL change leader", srv.patroni.lastLeader+" → "+leader)
		}
		if leader != "" {
			srv.patroni.lastLeader = leader
		}
		srv.patroni.mu.Unlock()
		pg["members"], pg["leader"], pg["reachable"] = members, leader, view != nil
	}

	// Status pages, from each host's latest report.
	probes := []ProbeView{}
	hosts := srv.store.Snapshot()
	for _, p := range dl.Probes {
		for _, h := range hosts {
			if len(p.From) > 0 && !slices.Contains(p.From, h.name) {
				continue
			}
			if len(p.From) == 0 && srv.workstation(h.name) {
				continue // workstations get only the probes that name them
			}
			if h.seen.IsZero() || now.Sub(h.seen) > srv.cfg.StaleAfter {
				continue // a silent host alerts by itself
			}
			var f *report.Fetch
			for i := range h.last.Fetches {
				if h.last.Fetches[i].ID == p.ID {
					f = &h.last.Fetches[i]
				}
			}
			v := ProbeView{ID: p.ID, Kind: p.Kind, Host: h.name, Status: "waiting", Word: "No reading yet"}
			if f != nil {
				srv.judgeProbe(p, h.name, f, &v, add)
			}
			probes = append(probes, v)
		}
	}
	sort.SliceStable(probes, func(i, j int) bool { return probes[i].Kind < probes[j].Kind })
	return map[string]any{"configured": true, "postgres": pg, "probes": probes, "lag_warn_mb": dl.LagWarnMB}, conds
}

func plainNetErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 && strings.Contains(msg, "dial tcp") {
		return msg[i+2:]
	}
	return msg
}

var kindName = map[string]string{"haproxy": "HAProxy", "nats": "NATS", "garage": "Garage", "etcd": "etcd"}

func (srv *Server) judgeProbe(p ProbeConfig, host string, f *report.Fetch, v *ProbeView, add func(key, sev, title, detail, host string)) {
	name := kindName[p.Kind]
	if f.Error != "" || f.Status == 0 {
		v.Status, v.Word = "critical", "Not answering"
		add(p.Kind+"/down/"+host, "critical", fmt.Sprintf("%s on %s isn't answering", name, host), "Its status page: "+f.Error+".", host)
		return
	}
	v.Status, v.Word = "good", "OK"
	switch p.Kind {
	case "garage":
		first, _, _ := strings.Cut(strings.TrimSpace(f.Body), "\n")
		v.Facts = map[string]any{"says": first}
		if f.Status != 200 || !strings.Contains(f.Body, "fully operational") {
			v.Status, v.Word = "critical", first
			add("garage/health/"+host, "critical", fmt.Sprintf("Garage on %s: %s", host, strings.TrimSuffix(first, ".")), "Its health page answered HTTP "+strconv.Itoa(f.Status)+".", host)
		}
	case "etcd":
		srv.judgeEtcd(p, host, f.Body, v, add)
	case "nats":
		var vz struct {
			Remotes       int    `json:"remotes"`
			Routes        int    `json:"routes"`
			Connections   int    `json:"connections"`
			SlowConsumers int64  `json:"slow_consumers"`
			InMsgs        int64  `json:"in_msgs"`
			OutMsgs       int64  `json:"out_msgs"`
			Uptime        string `json:"uptime"`
			Version       string `json:"version"`
		}
		if err := json.Unmarshal([]byte(f.Body), &vz); err != nil {
			v.Status, v.Word = "warning", "Unreadable"
			add("nats/unreadable/"+host, "warning", fmt.Sprintf("Can't read NATS's status on %s", host), err.Error(), host)
			return
		}
		v.Facts = map[string]any{"peers": vz.Remotes, "want_peers": p.Peers, "routes": vz.Routes, "connections": vz.Connections,
			"slow_consumers": vz.SlowConsumers, "in_msgs": vz.InMsgs, "out_msgs": vz.OutMsgs, "uptime": vz.Uptime, "version": vz.Version}
		if p.Peers > 0 && vz.Remotes < p.Peers {
			sev := "warning"
			if vz.Remotes == 0 {
				sev = "critical"
			}
			v.Status, v.Word = sev, fmt.Sprintf("%d of %d peers", vz.Remotes, p.Peers)
			add("nats/peers/"+host, sev, fmt.Sprintf("NATS on %s sees %d of %d peers", host, vz.Remotes, p.Peers), "A route to another NATS server is down.", host)
		}
		srv.patroni.mu.Lock() // reuse the lock for the small slow-consumer memory
		key := host + "/" + p.ID
		prev, seen := srv.natsSlow[key]
		srv.natsSlow[key] = vz.SlowConsumers
		srv.patroni.mu.Unlock()
		if seen && vz.SlowConsumers > prev {
			v.Status, v.Word = "warning", "Slow consumers"
			add("nats/slow/"+host, "warning", fmt.Sprintf("NATS on %s has slow consumers", host), fmt.Sprintf("%d so far, up from %d: a subscriber can't keep up.", vz.SlowConsumers, prev), host)
		}
	case "haproxy":
		bk, err := haproxyBackends(f.Body, p)
		if err != nil {
			v.Status, v.Word = "warning", "Unreadable"
			add("haproxy/unreadable/"+host, "warning", fmt.Sprintf("Can't read HAProxy's stats on %s", host), err.Error(), host)
			return
		}
		v.Backends = bk
		bad := 0
		for _, b := range bk {
			if b.Status == "good" {
				continue
			}
			bad++
			v.Status = worst(v.Status, b.Status)
			add("haproxy/"+b.Name+"/"+host, b.Status, fmt.Sprintf("HAProxy on %s: %s %s", host, b.Name, strings.ToLower(b.Word)),
				fmt.Sprintf("Up: %s. Down: %s.", orNone(b.Up), orNone(b.Down)), host)
		}
		if bad > 0 {
			v.Word = plural(bad, "backend in trouble", "backends in trouble")
		}
	}
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// haproxyBackends reads HAProxy's CSV stats and judges each backend.
func haproxyBackends(body string, p ProbeConfig) ([]HABackend, error) {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "# ")))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 1 {
		return nil, errors.New("not HAProxy's CSV")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	pi, si, sti := col["pxname"], col["svname"], col["status"]
	if _, ok := col["status"]; !ok {
		return nil, errors.New("no status column")
	}
	by := map[string]*HABackend{}
	var order []string
	backendDown := map[string]bool{}
	for _, row := range rows[1:] {
		if len(row) <= sti {
			continue
		}
		px, sv, st := row[pi], row[si], row[sti]
		if px == "stats" || sv == "FRONTEND" {
			continue
		}
		b := by[px]
		if b == nil {
			b = &HABackend{Name: px, Up: []string{}, Down: []string{}}
			by[px] = b
			order = append(order, px)
		}
		if sv == "BACKEND" {
			backendDown[px] = st != "UP"
			continue
		}
		switch {
		case strings.HasPrefix(st, "UP"):
			b.Up = append(b.Up, sv)
		case strings.HasPrefix(st, "DOWN"), st == "NOLB", st == "MAINT", strings.HasPrefix(st, "MAINT"):
			b.Down = append(b.Down, sv)
		}
	}
	out := []HABackend{}
	for _, name := range order {
		b := by[name]
		b.Status, b.Word = "good", "OK"
		switch {
		case slices.Contains(p.ExactlyOne, name):
			b.Rule = "exactly one"
			switch {
			case len(b.Up) == 0:
				b.Status, b.Word = "critical", "No server up"
			case len(b.Up) > 1:
				b.Status, b.Word = "critical", fmt.Sprintf("%d servers up, should be one", len(b.Up))
			}
		case slices.Contains(p.AtLeastOne, name):
			b.Rule = "at least one"
			if len(b.Up) == 0 {
				b.Status, b.Word = "critical", "No server up"
			}
		default:
			b.Rule = "all"
			switch {
			case backendDown[name] || len(b.Up) == 0:
				b.Status, b.Word = "critical", "Down"
			case len(b.Down) > 0:
				b.Status, b.Word = "warning", fmt.Sprintf("%d of %d servers down", len(b.Down), len(b.Up)+len(b.Down))
			}
		}
		out = append(out, *b)
	}
	return out, nil
}

func (srv *Server) handleDataLayer(w http.ResponseWriter, r *http.Request) {
	v, _ := srv.dataLayerView(time.Now())
	writeJSON(w, http.StatusOK, v)
}

// human is a duration as a person says it: 45 s, 3 min, 2 h 5 min.
func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// promValues reads "name value" lines (no labels) and histogram buckets
// (name{le="x"} value) from a Prometheus text page.
func promValues(body string) (vals map[string]float64, buckets map[string]map[float64]float64) {
	vals, buckets = map[string]float64{}, map[string]map[float64]float64{}
	for _, line := range strings.Split(body, "\n") {
		name, rest, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			continue
		}
		if base, labels, ok := strings.Cut(name, "{"); ok {
			le := strings.TrimSuffix(strings.TrimPrefix(labels, `le="`), `"}`)
			b, err := strconv.ParseFloat(le, 64)
			if le == "+Inf" {
				b, err = math.Inf(1), nil
			}
			if err == nil {
				if buckets[base] == nil {
					buckets[base] = map[float64]float64{}
				}
				buckets[base][b] = v
			}
			continue
		}
		vals[name] = v
	}
	return vals, buckets
}

// p99 is the upper bound of the bucket holding the 99th percentile of what
// happened between two readings of a cumulative histogram.
func p99(now, before map[float64]float64) (float64, float64) {
	bounds := make([]float64, 0, len(now))
	for b := range now {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)
	if len(bounds) == 0 {
		return 0, 0
	}
	total := now[bounds[len(bounds)-1]] - before[bounds[len(bounds)-1]]
	if total <= 0 {
		return 0, 0
	}
	for _, b := range bounds {
		if now[b]-before[b] >= 0.99*total {
			return b, total
		}
	}
	return bounds[len(bounds)-1], total
}

func (srv *Server) judgeEtcd(p ProbeConfig, host, body string, v *ProbeView, add func(key, sev, title, detail, host string)) {
	vals, buckets := promValues(body)
	hasLeader, ok := vals["etcd_server_has_leader"]
	if !ok {
		v.Status, v.Word = "warning", "Unreadable"
		add("etcd/unreadable/"+host, "warning", fmt.Sprintf("Can't read etcd's metrics on %s", host), "The metrics page had no etcd_server_has_leader.", host)
		return
	}
	size, quota := vals["etcd_mvcc_db_total_size_in_bytes"], vals["etcd_server_quota_backend_bytes"]
	facts := map[string]any{"has_leader": hasLeader == 1, "is_leader": vals["etcd_server_is_leader"] == 1,
		"leader_changes": vals["etcd_server_leader_changes_seen_total"], "db_bytes": size, "quota_bytes": quota}
	v.Facts = facts
	if hasLeader != 1 {
		v.Status, v.Word = "critical", "No leader"
		add("etcd/leader/"+host, "critical", fmt.Sprintf("etcd on %s has no leader", host), "Without a leader etcd takes no writes, so Patroni can't keep or change PostgreSQL's leader.", host)
	}
	warnPct := p.QuotaWarnPct
	if warnPct == 0 {
		warnPct = 75
	}
	if quota > 0 && size/quota*100 >= float64(warnPct) {
		v.Status, v.Word = worst(v.Status, "warning"), fmt.Sprintf("%.0f%% of quota", size/quota*100)
		add("etcd/quota/"+host, "warning", fmt.Sprintf("etcd on %s is at %.0f%% of its space quota", host, size/quota*100), "At 100% etcd stops taking writes; it needs compacting and defragmenting.", host)
	}
	// Disk sync time, over what happened since the last reading.
	now := buckets["etcd_disk_wal_fsync_duration_seconds_bucket"]
	srv.patroni.mu.Lock()
	before, seen := srv.etcdFsync[host]
	srv.etcdFsync[host] = now
	srv.patroni.mu.Unlock()
	if seen && now != nil {
		if q, n := p99(now, before); n >= 5 {
			facts["fsync_p99_ms"] = q * 1000
			warn := p.FsyncWarnMS
			if warn == 0 {
				warn = 25
			}
			if math.IsInf(q, 1) || q*1000 > float64(warn) {
				v.Status, v.Word = worst(v.Status, "warning"), "Slow disk syncs"
				add("etcd/fsync/"+host, "warning", fmt.Sprintf("etcd on %s: slow disk syncs", host),
					fmt.Sprintf("99%% of the last %d WAL syncs took up to %s (warns over %d ms): a slow or busy disk can cost etcd its leader.", int(n), fmtSecs(q), warn), host)
			}
		}
	}
}

func fmtSecs(s float64) string {
	if math.IsInf(s, 1) {
		return "over 8 s"
	}
	return fmt.Sprintf("%.0f ms", s*1000)
}
