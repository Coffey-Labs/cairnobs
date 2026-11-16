package mapgraph

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

var t0 = time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)

func key(n int) string { return fmt.Sprintf("key-%02d-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", n) }

// A world like ours: three hosts meshed over WireGuard, a desk machine on
// its own tunnel to node3, nginx taking the Internet, webmail behind it in
// a compose project, Patroni on all three, and a declared "mail" service.
func world() ([]Host, Corrections) {
	wg := func(self int, addr string, peers ...int) (report.Interface, report.WGDevice) {
		d := report.WGDevice{Name: "wg0", PublicKey: key(self), Peers: []report.WGPeer{}}
		for _, p := range peers {
			d.Peers = append(d.Peers, report.WGPeer{PublicKey: key(p), Handshake: t0.Add(-time.Minute)})
		}
		return report.Interface{Name: "wg0", Kind: "wireguard", Addrs: []string{addr}, Up: true}, d
	}
	patroni := report.Listener{Owner: "unit:patroni.service", Proto: "tcp", Addr: "*", Port: 5432}
	ssh := report.Listener{Owner: "unit:ssh.service", Proto: "tcp", Addr: "*", Port: 22}
	host := func(n int, extra *report.Map) Host {
		i, d := wg(n, fmt.Sprintf("10.20.0.%d/24", n), slices.DeleteFunc([]int{1, 2, 3}, func(p int) bool { return p == n })...)
		if n == 3 {
			d.Peers = append(d.Peers, report.WGPeer{PublicKey: key(9), Handshake: t0.Add(-10 * time.Minute)}) // desk, quiet
			d.Peers = append(d.Peers, report.WGPeer{PublicKey: key(77), Handshake: t0})                       // someone's laptop
		}
		m := &report.Map{
			Interfaces: []report.Interface{{Name: "eth0", Kind: "physical", Addrs: []string{fmt.Sprintf("198.51.100.%d/32", n)}, Up: true},
				{Name: "docker0", Kind: "container", Addrs: []string{"172.17.0.1/16"}, Up: true}, i},
			WireGuard: []report.WGDevice{d},
			Listen:    []report.Listener{ssh, patroni},
			Conns: []report.Conn{
				{Owner: "unit:ssh.service", Dir: "in", Port: 22, Count: 3}, // the Internet knocking: not a service
			},
		}
		if extra != nil {
			m.Listen = append(m.Listen, extra.Listen...)
			m.Conns = append(m.Conns, extra.Conns...)
		}
		return Host{Name: fmt.Sprintf("node%d", n), Map: m}
	}
	h1 := host(1, &report.Map{Listen: []report.Listener{{Owner: "container:stalwart", Proto: "tcp", Addr: "*", Port: 993}},
		Conns: []report.Conn{{Owner: "container:stalwart", Dir: "in", Port: 993, Count: 12}}})
	h1.Containers = []report.Container{{ID: "a1", Name: "stalwart", Image: "registry.example.org/mail/stalwart:0.16", State: "running"}}
	h2 := host(2, &report.Map{
		Listen: []report.Listener{
			{Owner: "unit:nginx.service", Proto: "tcp", Addr: "*", Port: 443},
			{Owner: "unit:docker.service", Proto: "tcp", Addr: "*", Port: 8080}, // docker-proxy for webmail
			{Owner: "container:webmail", Proto: "tcp", Addr: "*", Port: 80},
		},
		Conns: []report.Conn{
			{Owner: "unit:nginx.service", Dir: "in", Port: 443, Count: 40},
			{Owner: "unit:nginx.service", Dir: "out", Remote: "127.0.0.1", Port: 8080, Count: 5},
			{Owner: "container:webmail", Dir: "out", Remote: "10.20.0.1", Port: 5432, Count: 4},
			{Owner: "container:webmail", Dir: "out", Remote: "203.0.113.50", Port: 443, Count: 1},  // an outside API
			{Owner: "unit:patroni.service", Dir: "out", Remote: "10.20.0.1", Port: 5432, Count: 1}, // a replica: same service
		},
	})
	h2.Containers = []report.Container{{ID: "b1", Name: "webmail", Image: "registry.example.org/mail/webmail:3", Project: "webmail", State: "running",
		Ports: []report.PortMap{{Public: 8080, Private: 80, Proto: "tcp"}}, IPs: []string{"172.18.0.2"}}}
	h3 := host(3, nil)
	desk := Host{Name: "desk", Workstation: true, Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eno1", Kind: "physical", Addrs: []string{"192.168.50.63/24"}, Up: true},
			{Name: "wg-desk", Kind: "wireguard", Addrs: []string{"10.31.0.2/24"}, Up: true}},
		WireGuard: []report.WGDevice{{Name: "wg-desk", PublicKey: key(9), Peers: []report.WGPeer{{PublicKey: key(3), Handshake: t0.Add(-10 * time.Minute)}}}},
	}}
	cor := Corrections{SiteName: "Lisbon", SiteHosts: []string{"node1", "node2", "node3"}, Roles: map[string]string{"node1": "mail"},
		Services: []Declared{{ID: "mail", Name: "Mail", Containers: []string{"node1/stalwart*"}, Needs: []string{"unit:patroni", "node1"}},
			{ID: "cloudflare", Name: "Cloudflare", Tier: "edge"}}}
	return []Host{h1, h2, h3, desk}, cor
}

func TestBuildWorld(t *testing.T) {
	hosts, cor := world()
	g := Build(hosts, cor, t0)

	if len(g.Sites) != 1 || g.Sites[0].Name != "Lisbon" || !g.Sites[0].Mesh || !slices.Equal(g.Sites[0].Hosts, []string{"node1", "node2", "node3"}) {
		t.Errorf("sites = %+v", g.Sites)
	}
	node := map[string]HostNode{}
	for _, n := range g.Hosts {
		node[n.Name] = n
	}
	if b := node["desk"]; b.Site != "" || b.Via != "node3" {
		t.Errorf("desk = %+v, want remote via node3", b)
	}
	if n := node["node3"]; n.Outside != 1 {
		t.Errorf("node3 outside peers = %d, want 1 (the laptop)", n.Outside)
	}
	if n := node["node2"]; !slices.Equal(n.Internet, []int{443}) {
		t.Errorf("node2 internet = %v, want [443] (ssh knocks aren't a way in)", n.Internet)
	}

	var wgUp, wgDown, traffic []string
	for _, l := range g.Links {
		s := l.A + "-" + l.B
		switch {
		case l.Kind == "traffic":
			traffic = append(traffic, fmt.Sprint(s, l.Ports))
		case l.Up:
			wgUp = append(wgUp, s)
		default:
			wgDown = append(wgDown, s)
		}
	}
	if !slices.Equal(wgUp, []string{"node1-node2", "node1-node3", "node2-node3"}) || !slices.Equal(wgDown, []string{"desk-node3"}) {
		t.Errorf("wireguard up %v, down %v", wgUp, wgDown)
	}
	if !slices.Equal(traffic, []string{"node1-node2[5432]"}) {
		t.Errorf("traffic = %v", traffic)
	}

	svc := map[string]Service{}
	for _, s := range g.Services {
		svc[s.ID] = s
	}
	want := map[string]struct {
		tier  string
		layer int
		parts int
	}{
		"unit:nginx":      {"edge", 0, 1},
		"compose:webmail": {"apps", 1, 1},
		"unit:patroni":    {"data", 2, 3},
		"mail":            {"edge", 0, 1}, // the Internet reaches it on 993
		"cloudflare":      {"edge", 0, 0},
	}
	for id, w := range want {
		s, ok := svc[id]
		if !ok {
			t.Errorf("no service %s; have %v", id, g.Services)
			continue
		}
		if s.Tier != w.tier || s.Layer != w.layer || len(s.Parts) != w.parts {
			t.Errorf("%s: tier %s layer %d parts %d, want %+v", id, s.Tier, s.Layer, len(s.Parts), w)
		}
	}
	if len(svc) != len(want) {
		t.Errorf("services = %v", g.Services)
	}
	if s := svc["compose:webmail"]; !slices.Equal(s.Outside, []string{"203.0.113.50:443"}) {
		t.Errorf("webmail outside = %v", s.Outside)
	}
	if s := svc["unit:patroni"]; s.Kind != "postgresql" {
		t.Errorf("patroni kind %q", s.Kind)
	}
	var edges []string
	for _, e := range g.Edges {
		edges = append(edges, fmt.Sprintf("%s>%s:%d %s", e.From, e.To, e.Port, e.Source))
	}
	wantE := []string{"compose:webmail>unit:patroni:5432 learned", "mail>unit:patroni:0 declared", "unit:nginx>compose:webmail:8080 learned"}
	if !slices.Equal(edges, wantE) {
		t.Errorf("edges =\n%v\nwant\n%v", edges, wantE)
	}
}

// fleet makes n hosts in sites of 50, shaped like scripts/fleet's.
func fleet(n int) []Host {
	sites := (n + 49) / 50
	addr := func(site, pos int) string { return fmt.Sprintf("10.%d.%d.%d", 100+site/250, site%250, 10+pos) }
	var hosts []Host
	for i := range n {
		site, pos := i/50, i%50
		m := &report.Map{Interfaces: []report.Interface{{Name: "eth1", Kind: "vlan", Addrs: []string{addr(site, pos) + "/24"}, Up: true}}}
		var cs []report.Container
		switch {
		case pos == 0:
			m.Interfaces = append(m.Interfaces, report.Interface{Name: "wg0", Kind: "wireguard", Addrs: []string{fmt.Sprintf("10.250.%d.%d/16", site/250, site%250+1)}})
			d := report.WGDevice{Name: "wg0", PublicKey: key(1000 + site)}
			for o := range sites {
				if o != site {
					d.Peers = append(d.Peers, report.WGPeer{PublicKey: key(1000 + o), Handshake: t0})
				}
			}
			m.WireGuard = []report.WGDevice{d}
		case pos <= 2:
			m.Listen = []report.Listener{{Owner: "unit:haproxy.service", Proto: "tcp", Addr: "*", Port: 443}}
			m.Conns = append(m.Conns, report.Conn{Owner: "unit:haproxy.service", Dir: "in", Port: 443, Count: 40})
			for p := 3; p < 30; p++ {
				m.Conns = append(m.Conns, report.Conn{Owner: "unit:haproxy.service", Dir: "out", Remote: addr(site, p), Port: 8080, Count: 4})
			}
		case pos < 30:
			cs = []report.Container{{ID: "c", Name: "app-00", Image: "registry.example.org/app-00:1", Project: "stack", State: "running"}}
			m.Listen = []report.Listener{{Owner: "container:app-00", Proto: "tcp", Addr: "*", Port: 8080}}
			m.Conns = append(m.Conns,
				report.Conn{Owner: "container:app-00", Dir: "out", Remote: addr(site, 30+pos%10), Port: 5432, Count: 6},
				report.Conn{Owner: "container:app-00", Dir: "out", Remote: addr(site, 40+pos%5), Port: 6379, Count: 3},
				report.Conn{Owner: "container:app-00", Dir: "out", Remote: addr(site, 45+pos%5), Port: 4222, Count: 1})
		case pos < 40:
			m.Listen = []report.Listener{{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "*", Port: 5432}}
		case pos < 45:
			m.Listen = []report.Listener{{Owner: "unit:redis-server.service", Proto: "tcp", Addr: "*", Port: 6379}}
		default:
			m.Listen = []report.Listener{{Owner: "unit:nats-server.service", Proto: "tcp", Addr: "*", Port: 4222}}
		}
		hosts = append(hosts, Host{Name: fmt.Sprintf("fleet-%04d", i), Map: m, Containers: cs})
	}
	return hosts
}

func TestBuildFleet(t *testing.T) {
	start := time.Now()
	g := Build(fleet(1000), Corrections{}, t0)
	took := time.Since(start)
	if len(g.Sites) != 20 {
		t.Errorf("%d sites, want 20 (the gateways' WireGuard mustn't merge them)", len(g.Sites))
	}
	for _, s := range g.Sites {
		if len(s.Hosts) != 50 || s.Mesh {
			t.Errorf("site %s: %d hosts, mesh %v", s.ID, len(s.Hosts), s.Mesh)
			break
		}
	}
	wg := 0
	for _, l := range g.Links {
		if l.Kind == "wireguard" {
			wg++
		}
	}
	if wg != 20*19/2 {
		t.Errorf("%d WireGuard links, want 190", wg)
	}
	var names []string
	for _, s := range g.Services {
		names = append(names, fmt.Sprintf("%s/%s/%d", s.ID, s.Tier, s.Layer))
	}
	want := []string{"unit:haproxy/edge/0", "compose:stack/apps/1", "unit:nats-server/data/2", "unit:postgresql/data/2", "unit:redis-server/data/2"}
	if !slices.Equal(names, want) {
		t.Errorf("services = %v, want %v", names, want)
	}
	if len(g.Edges) != 4 {
		t.Errorf("edges = %v", g.Edges)
	}
	t.Logf("1,000 hosts: %d sites, %d links, %d services, %d edges in %s", len(g.Sites), len(g.Links), len(g.Services), len(g.Edges), took)
	if took > 2*time.Second {
		t.Errorf("took %s", took)
	}
}

func TestCorrectionsFromThePage(t *testing.T) {
	c := Corrections{Merged: map[string]string{"a": "b", "b": "a", "c": "d"}}
	if got := c.MergedInto("c"); got != "d" {
		t.Errorf("c → %s", got)
	}
	_ = c.MergedInto("a") // a loop ends, whatever it lands on
	o := Outside{Addrs: []string{"203.0.113.0/24", "198.51.100.7:25", "192.0.2.1"}}
	for _, tc := range []struct {
		addr string
		port int
		want bool
	}{{"203.0.113.9", 443, true}, {"198.51.100.7", 25, true}, {"198.51.100.7", 587, false}, {"192.0.2.1", 1, true}, {"192.0.2.2", 1, false}} {
		if got := o.Matches(netip.MustParseAddr(tc.addr), tc.port); got != tc.want {
			t.Errorf("%s:%d: %v", tc.addr, tc.port, got)
		}
	}
	if ValidAddr("example.org") || !ValidAddr("10.0.0.0/8") {
		t.Error("ValidAddr")
	}
	hosts := []Host{{Name: "a", Map: &report.Map{}}, {Name: "b", Map: &report.Map{}}}
	g := Build(hosts, Corrections{Hidden: map[string]bool{"host:b": true}}, time.Now())
	if len(g.Hosts) != 1 || g.Hosts[0].Name != "a" {
		t.Errorf("hidden host still drawn: %+v", g.Hosts)
	}
}

// A container's own Postgres listens on 5432 inside its namespace, as does
// Patroni on the host: a connection to the host's address on 5432 reaches
// Patroni, whichever the agent listed first (ours on prod, 2026-11-14:
// "Patroni needs Git" from Gitea's own database).
func TestResolveSkipsContainerListeners(t *testing.T) {
	inside := report.Listener{Owner: "container:gitea-db", Proto: "tcp", Addr: "*", Port: 5432, Inside: true}
	patroni := report.Listener{Owner: "unit:patroni.service", Proto: "tcp", Addr: "*", Port: 5432}
	h1 := Host{Name: "node1", Map: &report.Map{
		Interfaces: []report.Interface{{Name: "wg0", Kind: "wireguard", Addrs: []string{"10.20.0.1/24"}, Up: true}},
		Listen:     []report.Listener{inside, patroni},
	}, Containers: []report.Container{{ID: "c1", Name: "gitea-db", Image: "postgres:17", State: "running", IPs: []string{"172.18.0.3"}}}}
	h2 := Host{Name: "node2", Map: &report.Map{
		Interfaces: []report.Interface{{Name: "wg0", Kind: "wireguard", Addrs: []string{"10.20.0.2/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:haproxy.service", Proto: "tcp", Addr: "*", Port: 5000}},
		Conns:      []report.Conn{{Owner: "unit:haproxy.service", Dir: "out", Remote: "10.20.0.1", Port: 5432, Count: 2}},
	}}
	g := Build([]Host{h1, h2}, Corrections{}, t0)
	var to []string
	for _, e := range g.Edges {
		if e.Port == 5432 {
			to = append(to, e.To)
		}
	}
	if len(to) != 1 || !strings.Contains(to[0], "patroni") {
		t.Errorf("edges on 5432 go to %v, want Patroni alone", to)
	}
}

// Discourse's container talks to its own Redis over its own loopback; the
// host's own Redis on the same port isn't what it reaches.
func TestContainerLoopbackStaysInside(t *testing.T) {
	h := Host{Name: "node2", Map: &report.Map{
		Interfaces: []report.Interface{{Name: "wg0", Kind: "wireguard", Addrs: []string{"10.20.0.2/24"}, Up: true}},
		Listen: []report.Listener{
			{Owner: "unit:redis-server.service", Proto: "tcp", Addr: "127.0.0.1", Port: 6379},
			{Owner: "container:app", Proto: "tcp", Addr: "*", Port: 6379, Inside: true},
			{Owner: "container:app", Proto: "tcp", Addr: "*", Port: 80, Inside: true},
		},
		Conns: []report.Conn{{Owner: "container:app", Dir: "out", Remote: "127.0.0.1", Port: 6379, Count: 3, Inside: true}},
	}, Containers: []report.Container{
		{ID: "a", Name: "app", Image: "local_discourse/app", State: "running"},
	}}
	if g := Build([]Host{h}, Corrections{}, t0); len(g.Edges) != 0 {
		t.Errorf("edges %+v, want none", g.Edges)
	}
}

// A site whose hosts are named for it takes that name; a prefix two sites
// share, a mixed site and a small one keep their network's (#59).
func TestSiteNamedByHosts(t *testing.T) {
	sites := []Site{
		{Name: "10.100.0.0/24", Hosts: []string{"phx1-app01", "phx1-app02", "phx1-db01", "phx1-lb01", "build-box"}},
		{Name: "10.100.1.0/24", Hosts: []string{"iad1-app01", "iad1-db01", "iad1-gw01"}},
		{Name: "10.100.2.0/24", Hosts: []string{"fleet-0001", "fleet-0002", "fleet-0003"}},
		{Name: "10.100.3.0/24", Hosts: []string{"fleet-0101", "fleet-0102", "fleet-0103"}},
		{Name: "10.100.4.0/24", Hosts: []string{"ams1-app01", "ams1-db01", "web-1", "web-2"}},
		{Name: "10.100.5.0/24", Hosts: []string{"fra1-app01", "fra1-db01"}},
		{Name: "10.100.6.0/24", Hosts: []string{"10-0-0-1", "10-0-0-2", "10-0-0-3"}},
	}
	nameByHosts(sites)
	var got []string
	for _, s := range sites {
		got = append(got, s.Name)
	}
	want := []string{"phx1", "iad1", "10.100.2.0/24", "10.100.3.0/24", "10.100.4.0/24", "10.100.5.0/24", "10.100.6.0/24"}
	if !slices.Equal(got, want) {
		t.Errorf("names %v, want %v", got, want)
	}
	// A name set by hand still wins.
	hosts, cor := world()
	if g := Build(hosts, cor, t0); g.Sites[0].Name != "Lisbon" {
		t.Errorf("by hand: %s", g.Sites[0].Name)
	}
}
