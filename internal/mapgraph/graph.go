// Package mapgraph builds the maps from what the agents see (docs/maps.md):
// sites and networks from interfaces, links from WireGuard keys and
// traffic, services from containers, units and listeners, and what each
// service needs from the connections between them. It's pure: it takes
// the hosts' latest samples and corrections and returns a graph, so it can
// be tested at fleet size without a server.
package mapgraph

import (
	"cmp"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/netmap"
	"github.com/cairnobs/cairnobs/internal/report"
)

// Host is one agent's latest view of itself.
type Host struct {
	Name        string
	Map         *report.Map // nil: no sample (no helper, map: false, or not yet)
	Containers  []report.Container
	Workstation bool
}

// Corrections are what people have said about the maps: topology.yaml,
// and what's been corrected on the page (fixes.go).
type Corrections struct {
	// SiteName names the site that holds these hosts (topology.yaml's
	// network.site and its hosts).
	SiteName  string
	SiteHosts []string
	Roles     map[string]string // host → a short line of what it does
	Services  []Declared

	// From the page.
	Names     map[string]string // service or site id → the name people gave it
	Tiers     map[string]string // service id → edge, apps or data
	Hidden    map[string]bool   // service ids, and host:<name>
	Merged    map[string]string // service id → the service it's part of now
	Moved     map[string]string // PartKey → the service that part belongs to
	Confirmed []EdgeRef         // learned edges people confirmed: declared now
	Rejected  []EdgeRef         // not dependencies: never drawn or used
	Outside   []Outside
}

// Declared is a service someone wrote down. It claims the parts its
// patterns match, and its needs are declared edges.
type Declared struct {
	ID, Name, Tier string
	Needs          []string // service ids (host names are ignored here)
	Containers     []string // host/pattern, "*" for any host
	Units          []string // host/pattern; a unit without a type is a .service
}

// Graph is everything the maps draw, before grouping for a view.
type Graph struct {
	Built    time.Time
	Sites    []Site
	Hosts    []HostNode
	Links    []Link
	Services []Service
	Edges    []Edge
}

// Site is hosts joined by private networks.
type Site struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Networks []string `json:"networks"` // the prefixes that join it
	Hosts    []string `json:"hosts"`
	Mesh     bool     `json:"mesh,omitempty"` // joined only by a WireGuard mesh
}

// HostNode is a host on the network map.
type HostNode struct {
	Name     string   `json:"name"`
	Site     string   `json:"site,omitempty"` // empty: on its own, or remote
	Via      string   `json:"via,omitempty"`  // remote: the site host its link goes to
	Roles    string   `json:"roles,omitempty"`
	Addrs    []string `json:"addrs,omitempty"`
	Outside  int      `json:"outside_peers,omitempty"` // WireGuard peers that aren't our hosts
	Internet []int    `json:"internet,omitempty"`      // ports the Internet comes in on
	Mapped   bool     `json:"mapped"`                  // it sent a map sample
}

// Link joins two hosts.
type Link struct {
	A         string    `json:"a"`
	B         string    `json:"b"`
	Kind      string    `json:"kind"` // wireguard or traffic
	Iface     string    `json:"iface,omitempty"`
	Up        bool      `json:"up"`
	Handshake time.Time `json:"handshake,omitzero"`
	Ports     []int     `json:"ports,omitempty"` // traffic: the ports used, either way
}

// Service is a named group of parts.
type Service struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Source   string   `json:"source"` // compose, unit, image, process, declared
	Kind     string   `json:"kind,omitempty"`
	Parts    []Part   `json:"parts"`
	Ports    []int    `json:"ports,omitempty"`
	Internet []int    `json:"internet,omitempty"` // ports the Internet comes in on
	Outside  []string `json:"outside,omitempty"`  // addresses it reaches that aren't ours, addr:port
	Tier     string   `json:"tier"`               // edge, apps, data
	Layer    int      `json:"layer"`              // 0 at the top
	Declared bool     `json:"declared,omitempty"`
}

// Part is what a service is made of on one host.
type Part struct {
	Host  string `json:"host"`
	Owner string `json:"owner"` // container:<name>, unit:<name>, process:<name>
}

// Edge says From needs To, on Port.
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Port   int    `json:"port"`
	Count  int    `json:"count"`
	Source string `json:"source"` // learned or declared
}

// Plumbing is what every host runs and nobody depends on: never a
// service, and connections to it aren't dependencies.
var plumbing = map[string]bool{
	"unit:ssh.service": true, "unit:sshd.service": true, "process:sshd": true,
	"unit:systemd-resolved.service": true, "unit:systemd-networkd.service": true,
	"unit:avahi-daemon.service": true, "unit:cups.service": true, "unit:cups-browsed.service": true,
	"unit:rpcbind.service": true, "unit:chrony.service": true, "unit:chronyd.service": true,
	"unit:docker.service": true, "unit:containerd.service": true, "process:docker-proxy": true,
	"unit:obs-beacon-agent.service": true, "unit:obs-beacon-agent-helper.service": true,
	"unit:NetworkManager.service": true, "unit:dhcpcd.service": true, "process:dhclient": true,
	"unit:kubelet.service": true,
}

func isPlumbing(owner string) bool {
	return owner == "" || plumbing[owner] || strings.HasPrefix(owner, "unit:ifup@") || strings.HasPrefix(owner, "unit:systemd-")
}

// HandshakeFresh: a WireGuard link is up while its last handshake is this
// recent (WireGuard renews every two minutes while it carries traffic).
const HandshakeFresh = 3 * time.Minute

// Build makes the graph; clusters are the cluster agents' records
// (k8s.go).
func Build(hosts []Host, cor Corrections, now time.Time, clusters ...Cluster) *Graph {
	if len(cor.Hidden) > 0 {
		hosts = slices.DeleteFunc(slices.Clone(hosts), func(h Host) bool { return cor.Hidden["host:"+h.Name] })
	}
	b := newBuilder(hosts, cor)
	g := &Graph{Built: now}
	b.services()
	b.k8s(clusters)
	b.connections()
	g.Sites, g.Hosts = b.sites()
	g.Links = b.links(now)
	g.Services, g.Edges = b.finish()
	return g
}

type target struct {
	host, owner string
	port        int
}

type builder struct {
	hosts  []Host
	byName map[string]*Host
	cor    Corrections

	addrHost  map[netip.Addr]string            // our hosts' addresses
	ctrByIP   map[string]map[netip.Addr]string // host → container address → name
	ctrByPort map[string]map[int]string        // host → published port → container name
	ctr       map[string]map[string]report.Container

	listening map[string]bool             // owners that listen anywhere ("unit:x", "process:y")
	k8sAddr   map[netip.AddrPort][]string // a cluster Service's address → the map services behind it
	svc       map[string]*Service
	partSeen  map[string]bool
	edges     map[edgeKey]*Edge
	traffic   map[[2]string][]int // host pair → ports
	internet  map[string][]int    // host → ports
}

func newBuilder(hosts []Host, cor Corrections) *builder {
	b := &builder{hosts: hosts, cor: cor, byName: map[string]*Host{},
		addrHost: map[netip.Addr]string{}, ctrByIP: map[string]map[netip.Addr]string{},
		ctrByPort: map[string]map[int]string{}, ctr: map[string]map[string]report.Container{},
		listening: map[string]bool{}, svc: map[string]*Service{}, partSeen: map[string]bool{},
		edges: map[edgeKey]*Edge{}, traffic: map[[2]string][]int{}, internet: map[string][]int{}}
	for i := range hosts {
		h := &hosts[i]
		b.byName[h.Name] = h
		b.ctrByIP[h.Name], b.ctrByPort[h.Name], b.ctr[h.Name] = map[netip.Addr]string{}, map[int]string{}, map[string]report.Container{}
		for _, c := range h.Containers {
			b.ctr[h.Name][c.Name] = c
			for _, ip := range c.IPs {
				if a, err := netip.ParseAddr(ip); err == nil {
					b.ctrByIP[h.Name][a] = c.Name
				}
			}
			for _, p := range c.Ports {
				if p.Proto == "tcp" {
					b.ctrByPort[h.Name][p.Public] = c.Name
				}
			}
		}
		if h.Map == nil {
			continue
		}
		for _, ifc := range h.Map.Interfaces {
			if ifc.Kind == "container" {
				continue
			}
			for _, a := range ifc.Addrs {
				if pf, err := netip.ParsePrefix(a); err == nil {
					b.addrHost[pf.Addr()] = h.Name
				}
			}
		}
		for _, l := range h.Map.Listen {
			if !isPlumbing(l.Owner) {
				b.listening[l.Owner] = true
			}
		}
	}
	return b
}

// serviceFor says which service a host's socket owner belongs to, making
// it on first sight. ok is false for what isn't a service: plumbing, a
// unit or process that listens nowhere, a container we can't name.
func (b *builder) serviceFor(host, owner string) (*Service, bool) {
	if isPlumbing(owner) {
		return nil, false
	}
	kind, name, _ := strings.Cut(owner, ":")
	var id, label, source string
	if d, ok := b.declaredFor(host, kind, name); ok {
		id, label, source = d.ID, cmp.Or(d.Name, d.ID), "declared"
	} else {
		switch kind {
		case "container":
			c, ok := b.ctr[host][name]
			switch {
			case !ok:
				return nil, false // a Kubernetes container: its controller says (step 7)
			case c.Project != "":
				id, label, source = "compose:"+c.Project, c.Project, "compose"
			default:
				repo := imageRepo(c.Image)
				id, label, source = "image:"+repo, path.Base(repo), "image"
			}
		case "unit", "process":
			if !b.listening[owner] {
				return nil, false
			}
			label = strings.TrimSuffix(name, ".service")
			id, source = kind+":"+label, kind
		default:
			return nil, false
		}
	}
	return b.place(host, owner, id, label, source), true
}

// place puts a part in the service discovery found for it, or where
// people moved it: a part moved to another service, a service merged into
// another.
func (b *builder) place(host, owner, id, label, source string) *Service {
	to := b.cor.Moved[PartKey(host, owner)]
	if to == "" {
		to = b.cor.MergedInto(id)
	} else {
		to = b.cor.MergedInto(to)
	}
	if to != id {
		id, label, source = to, LabelOf(to), b.sourceOf(to)
		for _, d := range b.cor.Services {
			if d.ID == to {
				label = cmp.Or(d.Name, d.ID)
			}
		}
	}
	s := b.svc[id]
	if s == nil {
		s = &Service{ID: id, Name: label, Source: source, Declared: source == "declared"}
		b.svc[id] = s
	}
	if k := id + "\x00" + host + "\x00" + owner; !b.partSeen[k] {
		b.partSeen[k] = true
		s.Parts = append(s.Parts, Part{Host: host, Owner: owner})
	}
	return s
}

func (b *builder) declaredFor(host, kind, name string) (Declared, bool) {
	for _, d := range b.cor.Services {
		pats := d.Containers
		if kind == "unit" {
			pats = d.Units
		} else if kind != "container" {
			continue
		}
		for _, p := range pats {
			h, n, ok := strings.Cut(p, "/")
			if !ok || (h != "*" && h != host) {
				continue
			}
			if kind == "unit" && !strings.Contains(n, ".") {
				n += ".service"
			}
			if m, _ := path.Match(n, name); m {
				return d, true
			}
		}
	}
	return Declared{}, false
}

// imageRepo drops the registry's tag and digest: registry.example.org/team/app:1.2 → registry.example.org/team/app.
func imageRepo(img string) string {
	img, _, _ = strings.Cut(img, "@")
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		img = img[:i]
	}
	return img
}

// services makes a service of everything that listens or runs in a
// container, so a service shows even before anything talks to it.
func (b *builder) services() {
	for _, h := range b.hosts {
		if h.Workstation {
			continue
		}
		for _, c := range h.Containers {
			if c.State == "running" {
				b.serviceFor(h.Name, "container:"+c.Name)
			}
		}
		if h.Map == nil {
			continue
		}
		for _, l := range h.Map.Listen {
			if s, ok := b.serviceFor(h.Name, l.Owner); ok && !slices.Contains(s.Ports, l.Port) {
				s.Ports = append(s.Ports, l.Port)
			}
		}
	}
}

// resolve finds what an outgoing connection reaches: one of our hosts and
// what listens there on that port, or ok false for somewhere not ours.
func (b *builder) resolve(from string, remote netip.Addr, port int) (target, bool) {
	host := ""
	switch {
	case remote.IsLoopback():
		host = from
	case b.ctrByIP[from][remote] != "":
		return target{from, "container:" + b.ctrByIP[from][remote], port}, true
	default:
		host = b.addrHost[remote]
	}
	h := b.byName[host]
	if h == nil {
		return target{}, false
	}
	owner := ""
	if h.Map != nil {
		for _, l := range h.Map.Listen {
			// A container's own listener isn't at the host's address:
			// Postgres inside a container doesn't answer host:5432.
			if l.Port == port && l.Proto == "tcp" && !l.Inside {
				owner = l.Owner
				if !isPlumbing(owner) {
					break
				}
			}
		}
	}
	if isPlumbing(owner) { // Docker's proxy, or nothing: a published port
		if c := b.ctrByPort[host][port]; c != "" {
			owner = "container:" + c
		}
	}
	return target{host, owner, port}, true
}

func (b *builder) connections() {
	for _, h := range b.hosts {
		if h.Map == nil {
			continue
		}
		for _, c := range h.Map.Conns {
			if c.Dir == "in" {
				if c.Remote == "" { // from the Internet
					if !slices.Contains(b.internet[h.Name], c.Port) && !isPlumbing(b.listenerOwner(h, c.Port)) {
						b.internet[h.Name] = append(b.internet[h.Name], c.Port)
					}
					if s, ok := b.serviceFor(h.Name, c.Owner); ok && !slices.Contains(s.Internet, c.Port) {
						s.Internet = append(s.Internet, c.Port)
					}
				}
				continue
			}
			remote, err := netip.ParseAddr(c.Remote)
			if err != nil || isPlumbing(c.Owner) {
				continue
			}
			if c.Inside && remote.IsLoopback() {
				continue // within its own namespace: a container talking to itself
			}
			t, ours := b.resolve(h.Name, remote, c.Port)
			if !ours && len(b.k8sAddr[netip.AddrPortFrom(remote, uint16(c.Port))]) > 0 {
				// A cluster's Service, by its cluster or load-balancer address.
				if from, ok := b.serviceFor(h.Name, c.Owner); ok {
					for _, to := range b.k8sAddr[netip.AddrPortFrom(remote, uint16(c.Port))] {
						if to != from.ID {
							b.addEdge(from.ID, to, c.Port, c.Count)
						}
					}
				}
				continue
			}
			if !ours {
				if o, ok := b.outsideFor(remote, c.Port); ok {
					if from, ok := b.serviceFor(h.Name, c.Owner); ok {
						b.addEdge(from.ID, o.ID, c.Port, c.Count)
					}
					continue
				}
				if s, ok := b.serviceFor(h.Name, c.Owner); ok && !netmap.Private(remote) && len(s.Outside) < 20 {
					o := netip.AddrPortFrom(remote, uint16(c.Port)).String()
					if !slices.Contains(s.Outside, o) {
						s.Outside = append(s.Outside, o)
					}
				}
				continue
			}
			if t.host != h.Name && !isPlumbing(t.owner) {
				k := [2]string{min(h.Name, t.host), max(h.Name, t.host)}
				if !slices.Contains(b.traffic[k], c.Port) {
					b.traffic[k] = append(b.traffic[k], c.Port)
				}
			}
			from, ok1 := b.serviceFor(h.Name, c.Owner)
			to, ok2 := b.serviceFor(t.host, t.owner)
			if !ok1 || !ok2 || from == to {
				continue
			}
			b.addEdge(from.ID, to.ID, c.Port, c.Count)
		}
	}
}

func (b *builder) addEdge(from, to string, port, count int) {
	k := edgeKey{from, to, port}
	if e := b.edges[k]; e != nil {
		e.Count += count
	} else {
		b.edges[k] = &Edge{From: from, To: to, Port: port, Count: count, Source: "learned"}
	}
}

func (b *builder) listenerOwner(h Host, port int) string {
	for _, l := range h.Map.Listen {
		if l.Port == port {
			return l.Owner
		}
	}
	return ""
}

type edgeKey struct {
	from, to string
	port     int
}
