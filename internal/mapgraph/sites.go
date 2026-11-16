package mapgraph

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/cairnobs/cairnobs/internal/netmap"
)

// sites joins hosts that share a private network into sites. A WireGuard
// subnet joins hosts too, but only hosts that aren't in a site already,
// and only when three or more share it: a mesh. Two hosts on a tunnel of
// their own (a desk machine at home) is a link, not a site; that host is
// drawn as remote, beside the site at the other end.
func (b *builder) sites() ([]Site, []HostNode) {
	nets := map[netip.Prefix][]string{}
	wgNets := map[netip.Prefix][]string{}
	nodes := make([]HostNode, 0, len(b.hosts))
	for _, h := range b.hosts {
		n := HostNode{Name: h.Name, Roles: b.cor.Roles[h.Name], Mapped: h.Map != nil, Internet: b.internet[h.Name]}
		slices.Sort(n.Internet)
		if h.Map != nil {
			for _, ifc := range h.Map.Interfaces {
				if ifc.Kind == "container" {
					continue
				}
				for _, a := range ifc.Addrs {
					pf, err := netip.ParsePrefix(a)
					if err != nil {
						continue
					}
					n.Addrs = append(n.Addrs, pf.String())
					if !netmap.Private(pf.Addr()) || pf.Addr().IsLoopback() || pf.Bits() >= pf.Addr().BitLen()-1 {
						continue // public, or a /32 or /31: joins nothing
					}
					m := pf.Masked()
					if ifc.Kind == "wireguard" {
						wgNets[m] = appendNew(wgNets[m], h.Name)
					} else if ifc.Kind != "tun" {
						nets[m] = appendNew(nets[m], h.Name)
					}
				}
			}
		}
		nodes = append(nodes, n)
	}

	uf := newUnion()
	joinedBy := map[string][]string{} // root host → prefixes
	join := func(m map[netip.Prefix][]string, only func(string) bool) {
		for pf, hs := range m {
			var in []string
			for _, h := range hs {
				if only == nil || only(h) {
					in = append(in, h)
				}
			}
			if len(in) < 2 {
				continue
			}
			for _, h := range in[1:] {
				uf.union(in[0], h)
			}
			joinedBy[in[0]] = append(joinedBy[in[0]], pf.String())
		}
	}
	join(nets, nil)
	inSite := func(h string) bool { return uf.size(h) > 1 }
	before := map[string]bool{}
	for _, h := range b.hosts {
		before[h.Name] = inSite(h.Name)
	}
	mesh := map[netip.Prefix][]string{}
	for pf, hs := range wgNets {
		if len(hs) >= 3 {
			mesh[pf] = hs
		}
	}
	join(mesh, func(h string) bool { return !before[h] })

	// The topology's site name goes to the site holding most of its hosts.
	members := map[string][]string{}
	for _, h := range b.hosts {
		if inSite(h.Name) {
			r := uf.find(h.Name)
			members[r] = append(members[r], h.Name)
		}
	}
	var sites []Site
	siteOf := map[string]string{}
	for root, hs := range members {
		slices.Sort(hs)
		var prefixes []string
		for _, h := range hs {
			prefixes = append(prefixes, joinedBy[h]...)
		}
		slices.Sort(prefixes)
		prefixes = slices.Compact(prefixes)
		s := Site{ID: "site:" + prefixes[0], Name: prefixes[0], Networks: prefixes, Hosts: hs, Mesh: !before[root]}
		for _, h := range hs {
			siteOf[h] = s.ID
		}
		sites = append(sites, s)
	}
	nameByHosts(sites)
	if b.cor.SiteName != "" {
		best, most := -1, 0
		for i, s := range sites {
			n := 0
			for _, h := range b.cor.SiteHosts {
				if slices.Contains(s.Hosts, h) {
					n++
				}
			}
			if n > most {
				best, most = i, n
			}
		}
		if best >= 0 {
			sites[best].Name = b.cor.SiteName
		}
	}
	for i := range sites {
		if n := b.cor.Names[sites[i].ID]; n != "" {
			sites[i].Name = n
		}
	}
	slices.SortFunc(sites, func(a, b Site) int {
		return cmp.Or(cmp.Compare(len(b.Hosts), len(a.Hosts)), strings.Compare(a.ID, b.ID))
	})

	// Remote: a host in no site with a WireGuard link to a site host.
	peers := b.wgPeers()
	for i := range nodes {
		n := &nodes[i]
		n.Site = siteOf[n.Name]
		n.Outside = peers.outside[n.Name]
		if n.Site != "" {
			continue
		}
		for _, other := range peers.linked[n.Name] {
			if siteOf[other] != "" {
				n.Via = other
				break
			}
		}
	}
	return sites, nodes
}

func appendNew(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

type wgIndex struct {
	key     map[string]string   // public key → host
	linked  map[string][]string // host → our hosts it has as peers
	outside map[string]int      // host → peers that aren't ours
}

func (b *builder) wgPeers() wgIndex {
	w := wgIndex{key: map[string]string{}, linked: map[string][]string{}, outside: map[string]int{}}
	for _, h := range b.hosts {
		if h.Map == nil {
			continue
		}
		for _, d := range h.Map.WireGuard {
			if d.PublicKey != "" {
				w.key[d.PublicKey] = h.Name
			}
		}
	}
	for _, h := range b.hosts {
		if h.Map == nil {
			continue
		}
		for _, d := range h.Map.WireGuard {
			for _, p := range d.Peers {
				if o, ok := w.key[p.PublicKey]; ok && o != h.Name {
					w.linked[h.Name] = appendNew(w.linked[h.Name], o)
					w.linked[o] = appendNew(w.linked[o], h.Name)
				} else if !ok {
					w.outside[h.Name]++
				}
			}
		}
	}
	for h := range w.linked {
		slices.Sort(w.linked[h])
	}
	return w
}

// links: WireGuard links from matched keys (up while either end has a
// fresh handshake), and traffic links from connections between hosts.
func (b *builder) links(now time.Time) []Link {
	w := b.wgPeers()
	type pair struct{ a, b string }
	wg := map[pair]*Link{}
	for _, h := range b.hosts {
		if h.Map == nil {
			continue
		}
		for _, d := range h.Map.WireGuard {
			for _, p := range d.Peers {
				o, ok := w.key[p.PublicKey]
				if !ok || o == h.Name {
					continue
				}
				k := pair{min(h.Name, o), max(h.Name, o)}
				l := wg[k]
				if l == nil {
					l = &Link{A: k.a, B: k.b, Kind: "wireguard", Iface: d.Name}
					wg[k] = l
				}
				if p.Handshake.After(l.Handshake) {
					l.Handshake = p.Handshake
				}
				l.Up = !l.Handshake.IsZero() && now.Sub(l.Handshake) < HandshakeFresh
			}
		}
	}
	var out []Link
	for _, l := range wg {
		out = append(out, *l)
	}
	for k, ports := range b.traffic {
		slices.Sort(ports)
		out = append(out, Link{A: k[0], B: k[1], Kind: "traffic", Up: true, Ports: ports})
	}
	slices.SortFunc(out, func(x, y Link) int {
		return cmp.Or(strings.Compare(x.A, y.A), strings.Compare(x.B, y.B), strings.Compare(x.Kind, y.Kind))
	})
	return out
}

// finish adds what was declared, works out tiers and layers, and sorts.
func (b *builder) finish() ([]Service, []Edge) {
	for _, d := range b.cor.Services {
		if b.svc[d.ID] == nil {
			b.svc[d.ID] = &Service{ID: d.ID, Name: cmp.Or(d.Name, d.ID), Source: "declared", Declared: true}
		}
	}
	for _, o := range b.cor.Outside {
		b.outsideService(o)
	}
	declare := func(from, to string, port int) {
		if b.svc[from] == nil || b.svc[to] == nil || from == to {
			return // a host name, or nothing we know
		}
		if e := b.edges[edgeKey{from, to, port}]; e != nil {
			e.Source = "declared"
		} else {
			b.edges[edgeKey{from, to, port}] = &Edge{From: from, To: to, Port: port, Source: "declared"}
		}
	}
	for _, d := range b.cor.Services {
		for _, n := range d.Needs {
			declare(d.ID, n, 0)
		}
	}
	for _, o := range b.cor.Outside {
		for _, n := range o.NeededBy {
			declare(b.cor.MergedInto(n), o.ID, 0)
		}
	}
	for _, r := range b.cor.Confirmed {
		from, to := b.cor.MergedInto(r.From), b.cor.MergedInto(r.To)
		if e := b.edges[edgeKey{from, to, r.Port}]; e != nil || r.Port != 0 {
			declare(from, to, r.Port)
			continue
		}
		// Port 0: every edge between the two is confirmed, or one with no port.
		found := false
		for k, e := range b.edges {
			if k.from == from && k.to == to {
				e.Source, found = "declared", true
			}
		}
		if !found {
			declare(from, to, 0)
		}
	}
	for id := range b.svc {
		if b.cor.Hidden[id] {
			delete(b.svc, id)
		}
	}
	edges := make([]Edge, 0, len(b.edges))
	for _, e := range b.edges {
		if b.svc[e.From] == nil || b.svc[e.To] == nil || b.cor.Rejects(*e) {
			continue
		}
		edges = append(edges, *e)
	}
	slices.SortFunc(edges, func(x, y Edge) int {
		return cmp.Or(strings.Compare(x.From, y.From), strings.Compare(x.To, y.To), cmp.Compare(x.Port, y.Port))
	})

	out := make([]Service, 0, len(b.svc))
	for _, s := range b.svc {
		slices.Sort(s.Ports)
		slices.Sort(s.Internet)
		slices.Sort(s.Outside)
		slices.SortFunc(s.Parts, func(x, y Part) int { return cmp.Or(strings.Compare(x.Host, y.Host), strings.Compare(x.Owner, y.Owner)) })
		if s.Kind == "" {
			s.Kind = guessKind(s)
		}
		if n := b.cor.Names[s.ID]; n != "" {
			s.Name = n
		}
		out = append(out, *s)
	}
	tierAndLayer(out, edges, b.cor.Services, b.cor.Tiers)
	slices.SortFunc(out, func(x, y Service) int {
		return cmp.Or(cmp.Compare(x.Layer, y.Layer), strings.Compare(x.Name, y.Name), strings.Compare(x.ID, y.ID))
	})
	return out, edges
}

// tierAndLayer: what the Internet reaches is the edge; what only gets
// connected to is data, unless only the edge connects to it and it isn't
// a database, cache or queue by its kind (what a proxy or an Ingress sends
// to is an app); the rest are apps. Layers follow the longest path down
// from the top, so a service sits below everything that needs it. A
// declared tier wins.
func tierAndLayer(svcs []Service, edges []Edge, declared []Declared, tiers map[string]string) {
	idx := map[string]int{}
	for i, s := range svcs {
		idx[s.ID] = i
	}
	out := map[string][]string{}
	in := map[string][]string{}
	for _, e := range edges {
		out[e.From] = appendNew(out[e.From], e.To)
		in[e.To] = appendNew(in[e.To], e.From)
	}
	edge := func(id string) bool { i, ok := idx[id]; return ok && len(svcs[i].Internet) > 0 }
	for i := range svcs {
		s := &svcs[i]
		switch {
		case len(s.Internet) > 0:
			s.Tier = "edge"
		case len(out[s.ID]) == 0 && len(in[s.ID]) > 0 && (dataKinds[s.Kind] || !all(in[s.ID], edge)):
			s.Tier = "data"
		default:
			s.Tier = "apps"
		}
	}
	for _, d := range declared {
		if i, ok := idx[d.ID]; ok && d.Tier != "" {
			svcs[i].Tier = map[string]string{"edge": "edge", "public": "edge", "apps": "apps", "data": "data"}[d.Tier]
			if svcs[i].Tier == "" {
				svcs[i].Tier = "apps"
			}
		}
	}
	for i := range svcs {
		if t := tiers[svcs[i].ID]; t == "edge" || t == "apps" || t == "data" {
			svcs[i].Tier = t
		}
	}
	// Longest path from the top, by relaxing at most len(svcs) rounds; a
	// cycle (two services that need each other) stops growing there.
	layer := make([]int, len(svcs))
	for i, s := range svcs {
		if s.Tier == "data" {
			layer[i] = 1 // never on the top row
		}
	}
	for range len(svcs) {
		changed := false
		for _, e := range edges {
			f, okf := idx[e.From]
			t, okt := idx[e.To]
			if okf && okt && layer[t] < layer[f]+1 && layer[f]+1 < len(svcs) {
				layer[t], changed = layer[f]+1, true
			}
		}
		if !changed {
			break
		}
	}
	for i := range svcs {
		svcs[i].Layer = layer[i]
	}
}

// dataKinds are the kinds that hold or pass on data: always the data tier
// when nothing below them is needed.
var dataKinds = map[string]bool{"postgresql": true, "mysql": true, "redis": true, "mongodb": true, "nats": true, "kafka": true,
	"etcd": true, "memcached": true, "elasticsearch": true, "rabbitmq": true}

func all[T any](xs []T, f func(T) bool) bool {
	for _, x := range xs {
		if !f(x) {
			return false
		}
	}
	return true
}

var kindByPort = map[int]string{
	5432: "postgresql", 3306: "mysql", 6379: "redis", 27017: "mongodb", 4222: "nats", 9092: "kafka",
	2379: "etcd", 8008: "patroni", 11211: "memcached", 9200: "elasticsearch", 5672: "rabbitmq",
	25: "mail", 465: "mail", 587: "mail", 993: "mail", 143: "mail", 53: "dns",
	443: "web", 80: "web", 8080: "web", 3000: "web", 8443: "web",
}

// kindByName: a word in a service's name, and the kind it means.
var kindByName = []struct{ word, kind string }{
	{"postgres", "postgresql"}, {"patroni", "postgresql"}, {"mysql", "mysql"}, {"mariadb", "mysql"},
	{"redis", "redis"}, {"valkey", "redis"}, {"mongo", "mongodb"}, {"nats", "nats"}, {"kafka", "kafka"},
	{"etcd", "etcd"}, {"haproxy", "proxy"}, {"traefik", "proxy"}, {"nginx", "web"}, {"caddy", "web"},
	{"garage", "storage"}, {"minio", "storage"},
}

// guessKind says what a service is from its name, then its ports. It only
// picks an icon.
func guessKind(s *Service) string {
	n := strings.ToLower(s.Name)
	for _, k := range kindByName {
		if strings.Contains(n, k.word) {
			return k.kind
		}
	}
	for _, p := range s.Ports {
		if k := kindByPort[p]; k != "" {
			return k
		}
	}
	return ""
}

// union-find over host names, with sizes.
type union struct {
	parent map[string]string
	n      map[string]int // a root's size
}

func newUnion() *union { return &union{parent: map[string]string{}, n: map[string]int{}} }

func (u *union) find(x string) string {
	for {
		p, ok := u.parent[x]
		if !ok || p == x {
			return x
		}
		if gp, ok := u.parent[p]; ok {
			u.parent[x] = gp
		}
		x = p
	}
}

func (u *union) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if ra > rb {
		ra, rb = rb, ra
	}
	u.parent[rb], u.parent[ra] = ra, ra
	u.n[ra] = u.size(ra) + u.size(rb)
	delete(u.n, rb)
}

func (u *union) size(x string) int {
	if n, ok := u.n[u.find(x)]; ok {
		return n
	}
	return 1
}

// nameByHosts names a site after the prefix most of its hosts' names share
// before the first "-", as fleets named by site do (phx1-app05,
// phx1-db01): at least three hosts, four in five of them, and no other
// site with the same prefix. A name from topology.yaml or the map's
// corrections still wins; otherwise a site is named after its network.
func nameByHosts(sites []Site) {
	prefix := func(hs []string) string {
		if len(hs) < 3 {
			return ""
		}
		count := map[string]int{}
		for _, h := range hs {
			if p, _, ok := strings.Cut(h, "-"); ok && p != "" {
				count[p]++
			}
		}
		for p, n := range count {
			if n*5 >= len(hs)*4 && strings.ContainsFunc(p, unicode.IsLetter) {
				return p
			}
		}
		return ""
	}
	names := make([]string, len(sites))
	seen := map[string]int{}
	for i, s := range sites {
		names[i] = prefix(s.Hosts)
		seen[names[i]]++
	}
	for i, n := range names {
		if n != "" && seen[n] == 1 {
			sites[i].Name = n
		}
	}
}
