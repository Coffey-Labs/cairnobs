package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
	"github.com/cairnobs/cairnobs/internal/report"
)

func boxIDs(v *MapView) string {
	var ids []string
	for _, b := range v.Boxes {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, " ")
}

func lineKeys(v *MapView) string {
	var ls []string
	for _, l := range v.Lines {
		s := l.A + ">" + l.B + " " + l.Kind
		if l.Stage != "" {
			s += "/" + l.Stage
		}
		ls = append(ls, s)
	}
	slices.Sort(ls)
	return strings.Join(ls, ", ")
}

// Two hosts on one private network: nginx on host1 takes the Internet and
// talks to PostgreSQL on host2. The maps draw themselves from that.
func TestMapsFromReports(t *testing.T) {
	srv, _ := testServer(t, false)
	now := time.Now()
	h1 := report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{"10.0.0.1/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:nginx.service", Proto: "tcp", Addr: "*", Port: 443}},
		Conns: []report.Conn{{Owner: "unit:nginx.service", Dir: "in", Port: 443, Count: 30},
			{Owner: "unit:nginx.service", Dir: "out", Remote: "10.0.0.2", Port: 5432, Count: 2}},
	}}
	h2 := report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{"10.0.0.2/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "*", Port: 5432}},
	}}
	srv.store.Put("host1", h1, now)
	srv.store.Put("host2", h2, now)
	// The next report carries no map: the last one stays.
	srv.store.Put("host1", report.Report{V: report.Version}, now.Add(15*time.Second))
	if mp := srv.store.Snapshot()[0].mp; mp == nil || len(mp.Conns) != 2 {
		t.Fatalf("host1's map was dropped: %+v", mp)
	}
	now = now.Add(20 * time.Second)

	v, ok := srv.networkView("", now)
	if !ok {
		t.Fatal("no overview")
	}
	if got := boxIDs(v); got != "internet site:10.0.0.0/24" {
		t.Errorf("overview boxes: %s", got)
	}
	if got := lineKeys(v); got != "internet>site:10.0.0.0/24 internet" {
		t.Errorf("overview lines: %s", got)
	}
	if v.Mapped != 2 || v.Hosts != 2 {
		t.Errorf("mapped %d of %d", v.Mapped, v.Hosts)
	}
	v, _ = srv.networkView("site:10.0.0.0/24", now)
	if got := boxIDs(v); got != "internet host:host1 host:host2" {
		t.Errorf("site boxes: %s", got)
	}
	if got := lineKeys(v); got != "host:host1>host:host2 traffic, internet>host:host1 internet" {
		t.Errorf("site lines: %s", got)
	}
	v, _ = srv.networkView("host:host2", now)
	if got := boxIDs(v); got != "host:host2 host:host1" || len(v.Crumbs) != 2 {
		t.Errorf("host view: %s, crumbs %v", got, v.Crumbs)
	}
	if _, ok := srv.networkView("site:nowhere", now); ok {
		t.Error("a site that doesn't exist was found")
	}

	s, _ := srv.servicesView("", 3, now)
	if got := boxIDs(s); got != "svc:unit:nginx svc:unit:postgresql" {
		t.Errorf("services: %s", got)
	}
	if got := lineKeys(s); got != "svc:unit:nginx>svc:unit:postgresql needs/seen" {
		t.Errorf("service lines: %s", got)
	}
	if s.Boxes[0].Tier != "edge" || s.Boxes[1].Tier != "data" || s.Boxes[1].Layer != 1 {
		t.Errorf("tiers: %+v", s.Boxes)
	}

	// Seen in three separate hours: steady.
	for i := range 3 {
		srv.recordEdgesOnce(now.Add(time.Duration(i) * time.Hour))
	}
	srv.maps.graph = nil
	s, _ = srv.servicesView("svc:unit:postgresql", 3, now.Add(2*time.Hour))
	if got := lineKeys(s); got != "svc:unit:nginx>svc:unit:postgresql needs/steady" {
		t.Errorf("after three hours: %s", got)
	}
	// Not seen for two weeks, once nginx stops: fading, then gone.
	srv.store.Put("host1", report.Report{V: report.Version, Map: &report.Map{
		Interfaces: h1.Map.Interfaces, Listen: h1.Map.Listen}}, now.Add(time.Hour))
	srv.maps.graph = nil
	later := now.Add(16 * 24 * time.Hour)
	s, _ = srv.servicesView("", 3, later)
	if got := lineKeys(s); got != "svc:unit:nginx>svc:unit:postgresql needs/fading" {
		t.Errorf("two weeks on: %s", got)
	}
	srv.recordEdgesOnce(now.Add(31 * 24 * time.Hour)) // the purge, without a database
	srv.maps.graph = nil
	s, _ = srv.servicesView("", 3, now.Add(31*24*time.Hour))
	if got := lineKeys(s); got != "" {
		t.Errorf("a month on: %s", got)
	}

	// Finding things.
	rec := httptest.NewRecorder()
	srv.handleMapFind(rec, httptest.NewRequest("GET", "/api/v1/map/find?q=host2", nil))
	var found struct{ Found []MapFound }
	_ = json.Unmarshal(rec.Body.Bytes(), &found)
	if len(found.Found) != 1 || found.Found[0].Focus != "host:host2" || found.Found[0].Where != "10.0.0.0/24" {
		t.Errorf("find host2: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	srv.handleMapFind(rec, httptest.NewRequest("GET", "/api/v1/map/find?q=postgres", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &found)
	if len(found.Found) != 1 || found.Found[0].Focus != "svc:unit:postgresql" || found.Found[0].Map != "services" {
		t.Errorf("find postgres: %s", rec.Body)
	}

	// Through the handlers, signed in.
	rec = httptest.NewRecorder()
	srv.handleMapServices(rec, httptest.NewRequest("GET", "/api/v1/map/services?focus=svc:nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown service: %d", rec.Code)
	}
}

// A fleet of 1,000: the overview is twenty sites, a site opens on its
// hosts, the services are few, and each view comes back quickly.
func TestMapsAtFleetSize(t *testing.T) {
	srv, _ := testServer(t, false)
	now := time.Now()
	addr := func(site, pos int) string { return fmt.Sprintf("10.%d.%d.%d", 100+site/250, site%250, 10+pos) }
	for i := range 1000 {
		site, pos := i/50, i%50
		m := &report.Map{Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{addr(site, pos) + "/24"}, Up: true}}}
		switch {
		case pos < 3:
			m.Listen = []report.Listener{{Owner: "unit:haproxy.service", Proto: "tcp", Addr: "*", Port: 443}}
			m.Conns = []report.Conn{{Owner: "unit:haproxy.service", Dir: "in", Port: 443, Count: 9},
				{Owner: "unit:haproxy.service", Dir: "out", Remote: addr(site, 10), Port: 5432, Count: 1}}
		default:
			m.Listen = []report.Listener{{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "*", Port: 5432}}
		}
		srv.store.Put(fmt.Sprintf("fleet-%04d", i), report.Report{V: report.Version, Map: m}, now)
	}
	start := time.Now()
	v, _ := srv.networkView("", now)
	overview := time.Since(start)
	if n := strings.Count(boxIDs(v), "site:"); n != 20 {
		t.Errorf("%d sites on the overview", n)
	}
	start = time.Now()
	v, _ = srv.networkView("site:10.100.3.0/24", now)
	siteTook := time.Since(start)
	if n := strings.Count(boxIDs(v), "host:"); n != 50 {
		t.Errorf("%d hosts in a site", n)
	}
	start = time.Now()
	s, _ := srv.servicesView("", 3, now)
	svcTook := time.Since(start)
	if got := boxIDs(s); got != "svc:unit:haproxy svc:unit:postgresql" {
		t.Errorf("services: %s", got)
	}
	b, _ := json.Marshal(v)
	start = time.Now()
	conds := []Condition{{Key: "unit/fleet-0010/postgresql.service", Severity: "critical"}, {Key: "unit/fleet-0001/haproxy.service", Severity: "warning"}}
	srv.groupConditions(conds, never)
	groupTook := time.Since(start)
	t.Logf("1,000 hosts: overview %s, a site %s (%d KB), services %s, grouping %s", overview, siteTook, len(b)/1024, svcTook, groupTook)
	if overview > time.Second || siteTook > time.Second || svcTook > time.Second || groupTook > 200*time.Millisecond {
		t.Error("too slow")
	}
}

func TestGroupHosts(t *testing.T) {
	var hs []string
	for i := range 120 {
		hs = append(hs, fmt.Sprintf("web-%03d", i))
	}
	hs = append(hs, "db1", "db2", "gw")
	var got []string
	for _, g := range groupHosts("site:x", hs) {
		got = append(got, fmt.Sprintf("%s=%d", g.name, len(g.hosts)))
	}
	want := "db=2 gw=1 web-000 … web-049=50 web-050 … web-099=50 web-100 … web-119=20"
	if strings.Join(got, " ") != want {
		t.Errorf("groups: %v", got)
	}
}

// Corrections on the page: each changes the maps, is kept across a
// restart, and lists itself with its undo. Without a database nothing
// can be kept, so nothing is taken.
func TestMapCorrections(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req.AddCookie(cookie)
		req.Header.Set("X-Beacon", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	fix := func(req MapFixReq) {
		t.Helper()
		if rec := do("POST", "/api/v1/map/corrections", req); rec.Code != 200 {
			t.Fatalf("%+v: %d %s", req, rec.Code, rec.Body)
		}
	}
	now := time.Now()
	srv.store.Put("host1", report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth0", Kind: "physical", Addrs: []string{"10.0.0.1/24"}, Up: true}},
		Listen: []report.Listener{{Owner: "unit:nginx.service", Proto: "tcp", Addr: "*", Port: 443},
			{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "127.0.0.1", Port: 5432}},
		Conns: []report.Conn{{Owner: "unit:nginx.service", Dir: "out", Remote: "127.0.0.1", Port: 5432, Count: 3},
			{Owner: "unit:nginx.service", Dir: "out", Remote: "203.0.113.10", Port: 443, Count: 1}},
	}}, now)
	srv.recordEdgesOnce(now)
	view := func(focus string) *MapView {
		t.Helper()
		srv.maps.graph = nil
		v, ok := srv.servicesView(focus, 3, time.Now())
		if !ok {
			t.Fatalf("no view %q", focus)
		}
		return v
	}

	// What's new: both services and the edge between them.
	var found struct {
		Services []MapFoundService
		Edges    []MapFoundEdge
	}
	_ = json.Unmarshal(do("GET", "/api/v1/map/found", nil).Body.Bytes(), &found)
	if len(found.Services) != 2 || len(found.Edges) != 1 || found.Edges[0].ToName != "postgresql" {
		t.Fatalf("found: %+v", found)
	}
	_ = json.Unmarshal(do("GET", fmt.Sprintf("/api/v1/map/found?since=%d", time.Now().Add(time.Second).UnixMilli()), nil).Body.Bytes(), &found)
	if len(found.Services)+len(found.Edges) != 0 {
		t.Fatalf("found since now: %+v", found)
	}

	fix(MapFixReq{Op: "name", ID: "unit:postgresql", Name: "Main database"})
	fix(MapFixReq{Op: "tier", ID: "unit:nginx", Tier: "apps"})
	if v := view(""); v.Boxes[1].Name != "Main database" || v.Boxes[0].Tier != "apps" {
		t.Errorf("named and tiered: %+v", v.Boxes)
	}
	fix(MapFixReq{Op: "edge", From: "unit:nginx", To: "unit:postgresql", Port: 5432, Verdict: "reject"})
	if got := lineKeys(view("")); got != "" {
		t.Errorf("rejected, still drawn: %s", got)
	}
	_ = json.Unmarshal(do("GET", "/api/v1/map/found", nil).Body.Bytes(), &found)
	if len(found.Edges) != 0 {
		t.Errorf("rejected, still found: %+v", found.Edges)
	}
	fix(MapFixReq{Op: "edge", From: "unit:nginx", To: "unit:postgresql", Port: 5432, Verdict: "confirm"})
	if got := lineKeys(view("")); got != "svc:unit:nginx>svc:unit:postgresql needs/declared" {
		t.Errorf("confirmed: %s", got)
	}
	fix(MapFixReq{Op: "outside", Outside: &mapgraph.Outside{Name: "Payments", Addrs: []string{"203.0.113.10:443"}}})
	if got := lineKeys(view("svc:outside:payments")); !strings.Contains(got, "svc:unit:nginx>svc:outside:payments needs/seen") {
		t.Errorf("outside: %s", got)
	}
	fix(MapFixReq{Op: "move", Host: "host1", Owner: "unit:nginx.service", Name: "Front door"})
	if got := boxIDs(view("")); !strings.Contains(got, "svc:page:front-door") || strings.Contains(got, "svc:unit:nginx") {
		t.Errorf("moved: %s", got)
	}
	fix(MapFixReq{Op: "move", Host: "host1", Owner: "unit:nginx.service"})
	fix(MapFixReq{Op: "merge", ID: "unit:nginx", Into: "unit:postgresql"})
	if v := view(""); boxIDs(v) != "svc:unit:postgresql svc:outside:payments" || v.Boxes[0].Count != 2 { // the two as one, and Payments
		t.Errorf("merged: %s %+v", boxIDs(v), v.Boxes)
	}
	if rec := do("POST", "/api/v1/map/corrections", MapFixReq{Op: "merge", ID: "unit:postgresql", Into: "unit:postgresql"}); rec.Code != 400 {
		t.Errorf("merged into itself: %d", rec.Code)
	}
	fix(MapFixReq{Op: "merge", ID: "unit:nginx"})
	fix(MapFixReq{Op: "hide", ID: "unit:nginx"})
	if got := boxIDs(view("")); strings.Contains(got, "nginx") {
		t.Errorf("hidden: %s", got)
	}

	// Kept: a restart reads them back, and each lists with its undo.
	srv.Close()
	srv = stateServer(t, dir, true)
	h = srv.Handler()
	cookie = post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	var got struct {
		Editable bool
		Items    []MapFixItem
	}
	_ = json.Unmarshal(do("GET", "/api/v1/map/corrections", nil).Body.Bytes(), &got)
	var kinds []string
	for _, it := range got.Items {
		kinds = append(kinds, it.Kind)
	}
	if !got.Editable || strings.Join(kinds, " ") != "name tier hide confirm outside" {
		t.Errorf("after a restart: %+v", got)
	}
	for _, it := range got.Items {
		fix(it.Undo)
	}
	_ = json.Unmarshal(do("GET", "/api/v1/map/corrections", nil).Body.Bytes(), &got)
	if len(got.Items) != 0 {
		t.Errorf("after undoing each: %+v", got.Items)
	}

	// No database: nothing taken.
	plain, _ := testServer(t, true)
	ph := plain.Handler()
	c := post(ph, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	req := httptest.NewRequest("POST", "/api/v1/map/corrections", strings.NewReader(`{"op":"hide","id":"host:host1"}`))
	req.AddCookie(c)
	req.Header.Set("X-Beacon", "1")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("without a database: %d %s", rec.Code, rec.Body)
	}
}

// A cluster's services are one box on the service map's overview, opening
// into them; a host calling a Service's address needs that box.
func TestClusterOnTheServiceMap(t *testing.T) {
	a, _, _, clusterTok := clusterCopies(t)
	now := time.Now()
	recs := append(smallCluster(now),
		report.Workload{Kind: report.KindService, ID: "s1", Name: "web", Namespace: "shop", State: report.StateOK, Word: "2 ready endpoints",
			Facts: map[string]any{"type": "ClusterIP", "cluster_ips": "10.96.0.10", "ports": "80", "backends": "deployment/web"}},
		report.Workload{Kind: report.KindIngress, ID: "i1", Name: "ingress/shop", Namespace: "shop", State: report.StateOK, Word: "shop.example.org → web",
			Facts: map[string]any{"hosts": "shop.example.org", "backends": "web:80", "tls": true}})
	sendBatch(t, a, clusterTok, report.Workloads{Full: "begin", Records: recs})
	sendBatch(t, a, clusterTok, report.Workloads{Full: "end"})
	a.store.Put("host1", report.Report{V: report.Version, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{"10.0.0.1/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:nginx.service", Proto: "tcp", Addr: "*", Port: 443}},
		Conns:      []report.Conn{{Owner: "unit:nginx.service", Dir: "out", Remote: "10.96.0.10", Port: 80, Count: 3}},
	}}, now)

	v, ok := a.servicesView("", 1, now)
	if !ok {
		t.Fatal("no overview")
	}
	ids := boxIDs(v)
	if !strings.Contains(ids, "group:cluster:prod") || strings.Contains(ids, "svc:k8s:") {
		t.Errorf("overview boxes: %s", ids)
	}
	for _, b := range v.Boxes {
		if b.ID == "group:cluster:prod" && (b.Sub != "Kubernetes cluster" || b.Count != 3) {
			t.Errorf("cluster box: %+v", b)
		}
	}
	if l := lineKeys(v); !strings.Contains(l, ">group:cluster:prod needs") {
		t.Errorf("overview lines: %s", l)
	}

	v, ok = a.servicesView("group:cluster:prod", 1, now)
	if !ok {
		t.Fatal("no cluster view")
	}
	if ids := boxIDs(v); !strings.Contains(ids, "svc:k8s:prod/shop/web") || !strings.Contains(ids, "svc:k8s:prod/ingress") {
		t.Errorf("cluster boxes: %s", ids)
	}
	if l := lineKeys(v); !strings.Contains(l, "svc:k8s:prod/ingress>svc:k8s:prod/shop/web needs/declared") {
		t.Errorf("cluster lines: %s", l)
	}

	v, ok = a.servicesView("svc:k8s:prod/shop/web", 1, now)
	if !ok {
		t.Fatal("no service view")
	}
	if len(v.Crumbs) != 2 || v.Crumbs[1].ID != "group:cluster:prod" {
		t.Errorf("crumbs: %+v", v.Crumbs)
	}

	// A cluster's services come from its API, not a guess: they aren't
	// listed as found, so a new cluster doesn't flood the list.
	a.recordEdgesOnce(now)
	h := a.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/api/v1/map/found", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Beacon", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var found struct{ Services []MapFoundService }
	_ = json.Unmarshal(rec.Body.Bytes(), &found)
	if len(found.Services) != 1 || found.Services[0].Name != "nginx" {
		t.Errorf("found: %s", rec.Body)
	}

	// A correction about a cluster's service names the cluster: two often
	// run an app of the same name.
	body, _ := json.Marshal(MapFixReq{Op: "edge", From: "unit:nginx", To: "k8s:prod/shop/web", Port: 80, Verdict: "confirm"})
	req = httptest.NewRequest("POST", "/api/v1/map/corrections", strings.NewReader(string(body)))
	req.AddCookie(cookie)
	req.Header.Set("X-Beacon", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("GET", "/api/v1/map/corrections", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Beacon", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var fixes struct{ Items []MapFixItem }
	_ = json.Unmarshal(rec.Body.Bytes(), &fixes)
	if len(fixes.Items) != 1 || fixes.Items[0].Text != "nginx needs web (prod) on :80" {
		t.Errorf("corrections: %s", rec.Body)
	}
}
