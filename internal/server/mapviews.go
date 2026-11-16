package server

// The maps' API: one level at a time, already grouped, so a view never
// sends more than mapMaxBoxes boxes (docs/maps.md, Drawing it at scale).
//
//	GET /api/v1/map/network                    sites, clusters, remote hosts, the Internet
//	GET /api/v1/map/network?focus=site:<id>    a site's hosts (grouped past mapSiteCards)
//	GET /api/v1/map/network?focus=group:<id>   one group of a site's hosts
//	GET /api/v1/map/network?focus=host:<name>  a host and what it's linked to
//	GET /api/v1/map/services                   every service (grouped past mapServiceCards)
//	GET /api/v1/map/services?focus=group:<id>  one group of services
//	GET /api/v1/map/services?focus=svc:<id>&steps=3
//	                                           a service, what it needs and what needs it

import (
	"cmp"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
)

const (
	mapMaxBoxes     = 300
	mapMaxLines     = 1000
	mapSiteCards    = 60  // hosts drawn one by one in a site before grouping
	mapServiceCards = 150 // services drawn one by one before grouping
	mapGroupSize    = 50  // hosts per group when nothing else splits them
	mapDetailList   = 25  // parts and hosts listed in a box's detail
)

// MapBox is one box on a map.
type MapBox struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // internet, site, cluster, host, group, service
	Name     string   `json:"name"`
	Sub      string   `json:"sub,omitempty"`
	Status   string   `json:"status"` // good, warning, critical, waiting
	Count    int      `json:"count,omitempty"`
	Problems []string `json:"problems,omitempty"` // names of the members in trouble, a few
	Ports    []int    `json:"ports,omitempty"`
	Open     bool     `json:"open,omitempty"` // a focus exists for it
	Tier     string   `json:"tier,omitempty"`
	Layer    int      `json:"layer"`
	Site     string   `json:"site,omitempty"`
	Via      string   `json:"via,omitempty"`
	Detail   any      `json:"detail,omitempty"`
}

// MapLine is one line between two boxes.
type MapLine struct {
	A     string `json:"a"`
	B     string `json:"b"`
	Kind  string `json:"kind"` // wireguard, traffic, internet, needs
	Up    bool   `json:"up"`
	Ports []int  `json:"ports,omitempty"`
	Count int    `json:"count,omitempty"` // links or edges folded into it
	Stage string `json:"stage,omitempty"` // needs: declared, seen, steady, fading
}

// MapView is one level of a map.
type MapView struct {
	Built  time.Time `json:"built"`
	Level  string    `json:"level"`
	Focus  string    `json:"focus,omitempty"`
	Title  string    `json:"title"` // what the view shows: a site's name, a host's
	Crumbs []MapBox  `json:"crumbs"`
	Boxes  []MapBox  `json:"boxes"`
	Lines  []MapLine `json:"lines"`
	Cut    bool      `json:"cut,omitempty"` // more than the caps: the rest left out
	Hosts  int       `json:"hosts"`
	Mapped int       `json:"mapped"` // hosts that sent a map sample
}

var statusOrder = map[string]int{"good": 0, "waiting": 1, "warning": 2, "critical": 3}

func worse(a, b string) string {
	if statusOrder[b] > statusOrder[a] {
		return b
	}
	return a
}

// fold makes a group box out of members: the worst status, the count and
// the first few members in trouble.
func fold(id, kind, name string, members []string, status map[string]string) MapBox {
	b := MapBox{ID: id, Kind: kind, Name: name, Status: "good", Count: len(members), Open: true}
	if len(members) == 0 {
		b.Status = "waiting"
	}
	for _, m := range members {
		s := cmp.Or(status[m], "waiting")
		b.Status = worse(b.Status, s)
		if (s == "critical" || s == "warning") && len(b.Problems) < 5 {
			b.Problems = append(b.Problems, m)
		}
	}
	return b
}

func (v *MapView) capped() {
	if len(v.Boxes) > mapMaxBoxes {
		v.Boxes, v.Cut = v.Boxes[:mapMaxBoxes], true
	}
	keep := map[string]bool{}
	for _, b := range v.Boxes {
		keep[b.ID] = true
	}
	v.Lines = slices.DeleteFunc(v.Lines, func(l MapLine) bool { return !keep[l.A] || !keep[l.B] })
	if len(v.Lines) > mapMaxLines {
		v.Lines, v.Cut = v.Lines[:mapMaxLines], true
	}
	if v.Lines == nil {
		v.Lines = []MapLine{}
	}
	if v.Boxes == nil {
		v.Boxes = []MapBox{}
	}
	if v.Crumbs == nil {
		v.Crumbs = []MapBox{}
	}
}

// lines folds links between members into lines between their boxes.
type lineSet struct {
	m     map[[3]string]*MapLine
	order [][3]string
}

func newLineSet() *lineSet { return &lineSet{m: map[[3]string]*MapLine{}} }

func (s *lineSet) add(a, b, kind string, up bool, ports []int, stage string) {
	if a == "" || b == "" || a == b {
		return
	}
	if kind != "needs" && kind != "internet" && a > b {
		a, b = b, a
	}
	k := [3]string{a, b, kind}
	l := s.m[k]
	if l == nil {
		l = &MapLine{A: a, B: b, Kind: kind, Up: up, Stage: stage}
		s.m[k] = l
		s.order = append(s.order, k)
	} else {
		l.Up = l.Up && up
		if stageRank[stage] > stageRank[l.Stage] {
			l.Stage = stage
		}
	}
	l.Count++
	for _, p := range ports {
		if !slices.Contains(l.Ports, p) {
			l.Ports = append(l.Ports, p)
		}
	}
	slices.Sort(l.Ports)
}

var stageRank = map[string]int{stageFading: 0, stageSeen: 1, stageSteady: 2, stageDeclared: 3}

func (s *lineSet) list() []MapLine {
	out := make([]MapLine, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, *s.m[k])
	}
	return out
}

// hostStatus is every host's status now.
func (srv *Server) hostStatus(now time.Time) (map[string]string, map[string]HostView) {
	hosts, _ := srv.view(now)
	st := make(map[string]string, len(hosts))
	by := make(map[string]HostView, len(hosts))
	for _, h := range hosts {
		st[h.Name], by[h.Name] = h.Status, h
	}
	return st, by
}

func (srv *Server) handleMapNetwork(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	v, ok := srv.networkView(r.URL.Query().Get("focus"), now)
	if !ok {
		http.Error(w, "no such place on the map", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (srv *Server) networkView(focus string, now time.Time) (*MapView, bool) {
	g := srv.mapGraph(now)
	status, views := srv.hostStatus(now)
	v := &MapView{Built: g.Built, Focus: focus, Hosts: len(g.Hosts)}
	node := map[string]mapgraph.HostNode{}
	for _, n := range g.Hosts {
		node[n.Name] = n
		if n.Mapped {
			v.Mapped++
		}
	}
	site := map[string]mapgraph.Site{}
	for _, s := range g.Sites {
		site[s.ID] = s
	}
	hostBox := func(n mapgraph.HostNode) MapBox {
		b := MapBox{ID: "host:" + n.Name, Kind: "host", Name: n.Name, Sub: n.Roles, Status: cmp.Or(status[n.Name], "waiting"),
			Ports: n.Internet, Open: true, Site: n.Site, Via: n.Via}
		if hv, ok := views[n.Name]; ok && b.Sub == "" {
			b.Sub = hv.Word
		}
		return b
	}
	overview := MapBox{ID: "", Kind: "overview", Name: "Everything"}
	ls := newLineSet()

	switch kind, id, _ := strings.Cut(focus, ":"); kind {
	case "":
		v.Level, v.Title = "overview", "Everything"
		// Where each host is drawn: its site, or itself.
		where := map[string]string{}
		internet := false
		for _, s := range g.Sites {
			b := fold(s.ID, "site", s.Name, s.Hosts, status)
			b.Sub = fmt.Sprintf("%d hosts", len(s.Hosts))
			if s.Mesh {
				b.Sub += " · WireGuard mesh"
			}
			var ports []int
			for _, h := range s.Hosts {
				where[h] = b.ID
				for _, p := range node[h].Internet {
					if !slices.Contains(ports, p) {
						ports = append(ports, p)
					}
				}
			}
			slices.Sort(ports)
			b.Ports = ports
			if len(ports) > 0 {
				internet = true
				ls.add("internet", b.ID, "internet", true, ports, "")
			}
			v.Boxes = append(v.Boxes, b)
		}
		var alone []string
		for _, n := range g.Hosts {
			if n.Site != "" {
				continue
			}
			if n.Via != "" {
				b := hostBox(n)
				where[n.Name] = b.ID
				v.Boxes = append(v.Boxes, b)
			} else {
				alone = append(alone, n.Name)
			}
			if len(n.Internet) > 0 {
				internet = true
			}
		}
		if len(alone) > 0 && len(alone) <= 30 {
			for _, h := range alone {
				b := hostBox(node[h])
				where[h] = b.ID
				v.Boxes = append(v.Boxes, b)
				if len(node[h].Internet) > 0 {
					ls.add("internet", b.ID, "internet", true, node[h].Internet, "")
				}
			}
		} else if len(alone) > 30 {
			b := fold("group:alone", "group", "Hosts on their own", alone, status)
			for _, h := range alone {
				where[h] = b.ID
			}
			v.Boxes = append(v.Boxes, b)
		}
		if internet {
			v.Boxes = append([]MapBox{{ID: "internet", Kind: "internet", Name: "Internet", Status: "good"}}, v.Boxes...)
		}
		for _, l := range g.Links {
			ls.add(where[l.A], where[l.B], l.Kind, l.Up, l.Ports, "")
		}
		_, conds := srv.view(now)
		for _, c := range srv.clusterViews(now, conds) {
			v.Boxes = append(v.Boxes, MapBox{ID: "cluster:" + c.Name, Kind: "cluster", Name: c.Name, Status: c.Status,
				Sub: c.Summary, Count: c.Nodes})
		}

	case "site", "group":
		hosts, title, ok := srv.siteHosts(g, kind, id)
		if !ok {
			return nil, false
		}
		v.Level, v.Title = kind, title
		v.Crumbs = []MapBox{overview}
		if kind == "group" {
			if s, ok := site[groupSite(id)]; ok {
				v.Crumbs = append(v.Crumbs, MapBox{ID: s.ID, Kind: "site", Name: s.Name})
			}
		}
		where := map[string]string{}
		if len(hosts) <= mapSiteCards || kind == "group" {
			for _, h := range hosts {
				b := hostBox(node[h])
				where[h] = b.ID
				v.Boxes = append(v.Boxes, b)
			}
		} else {
			for _, grp := range groupHosts(id, hosts) {
				b := fold("group:"+grp.id, "group", grp.name, grp.hosts, status)
				for _, h := range grp.hosts {
					where[h] = b.ID
				}
				v.Boxes = append(v.Boxes, b)
			}
		}
		// Links out of the box go to where the other end is drawn on the
		// overview: its site, or itself.
		inside := map[string]bool{}
		for _, h := range hosts {
			inside[h] = true
		}
		elsewhere := map[string]MapBox{}
		outside := func(h string) string {
			n := node[h]
			b := hostBox(n)
			if n.Site != "" {
				s := site[n.Site]
				b = fold(s.ID, "site", s.Name, s.Hosts, status)
			}
			b.Kind = "elsewhere:" + b.Kind
			elsewhere[b.ID] = b
			return b.ID
		}
		var ports []int
		for _, h := range hosts {
			for _, p := range node[h].Internet {
				if !slices.Contains(ports, p) {
					ports = append(ports, p)
				}
				ls.add("internet", where[h], "internet", true, node[h].Internet, "")
			}
		}
		if len(ports) > 0 {
			v.Boxes = append([]MapBox{{ID: "internet", Kind: "internet", Name: "Internet", Status: "good"}}, v.Boxes...)
		}
		for _, l := range g.Links {
			a, b := inside[l.A], inside[l.B]
			switch {
			case a && b:
				ls.add(where[l.A], where[l.B], l.Kind, l.Up, l.Ports, "")
			case a:
				ls.add(where[l.A], outside(l.B), l.Kind, l.Up, l.Ports, "")
			case b:
				ls.add(outside(l.A), where[l.B], l.Kind, l.Up, l.Ports, "")
			}
		}
		for _, b := range sortedBoxes(elsewhere) {
			v.Boxes = append(v.Boxes, b)
		}

	case "host":
		n, ok := node[id]
		if !ok {
			return nil, false
		}
		v.Level, v.Title = "host", n.Name
		v.Crumbs = []MapBox{overview}
		if n.Site != "" {
			s := site[n.Site]
			v.Crumbs = append(v.Crumbs, MapBox{ID: s.ID, Kind: "site", Name: s.Name})
		}
		b := hostBox(n)
		var svcs []string
		for _, s := range g.Services {
			for _, p := range s.Parts {
				if p.Host == id {
					svcs = append(svcs, s.ID)
					break
				}
			}
		}
		b.Detail = map[string]any{"addrs": n.Addrs, "outside_peers": n.Outside, "services": svcs, "mapped": n.Mapped}
		v.Boxes = append(v.Boxes, b)
		seen := map[string]bool{id: true}
		for _, l := range g.Links {
			other := ""
			switch id {
			case l.A:
				other = l.B
			case l.B:
				other = l.A
			default:
				continue
			}
			if !seen[other] {
				seen[other] = true
				v.Boxes = append(v.Boxes, hostBox(node[other]))
			}
			ls.add(b.ID, "host:"+other, l.Kind, l.Up, l.Ports, "")
		}
		if len(n.Internet) > 0 {
			v.Boxes = append([]MapBox{{ID: "internet", Kind: "internet", Name: "Internet", Status: "good"}}, v.Boxes...)
			ls.add("internet", b.ID, "internet", true, n.Internet, "")
		}
	default:
		return nil, false
	}
	v.Lines = ls.list()
	v.capped()
	return v, true
}

func sortedBoxes(m map[string]MapBox) []MapBox {
	out := make([]MapBox, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b MapBox) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// A group of a site's hosts is "<site id>|<key>".
func groupSite(id string) string { s, _, _ := strings.Cut(id, "|"); return s }

func (srv *Server) siteHosts(g *mapgraph.Graph, kind, id string) ([]string, string, bool) {
	if kind == "group" && id == "alone" {
		var hs []string
		for _, n := range g.Hosts {
			if n.Site == "" && n.Via == "" {
				hs = append(hs, n.Name)
			}
		}
		return hs, "Hosts on their own", len(hs) > 0
	}
	for _, s := range g.Sites {
		if kind == "site" && s.ID == "site:"+id {
			return s.Hosts, s.Name, true
		}
		if kind == "group" && s.ID == groupSite(id) {
			for _, grp := range groupHosts(s.ID, s.Hosts) {
				if grp.id == id {
					return grp.hosts, grp.name, true
				}
			}
		}
	}
	return nil, "", false
}

type hostGroup struct {
	id, name string
	hosts    []string
}

var trailingNumber = regexp.MustCompile(`[-_.]?\d+$`)

// groupHosts splits a big site: by name pattern (web-01 … web-40 is "web"),
// and anything still too big into runs of mapGroupSize in name order.
func groupHosts(siteID string, hosts []string) []hostGroup {
	byPat := map[string][]string{}
	var pats []string
	for _, h := range hosts {
		p := trailingNumber.ReplaceAllString(h, "")
		if p == "" {
			p = h
		}
		if byPat[p] == nil {
			pats = append(pats, p)
		}
		byPat[p] = append(byPat[p], h)
	}
	slices.Sort(pats)
	var out []hostGroup
	for _, p := range pats {
		hs := byPat[p]
		slices.Sort(hs)
		for i := 0; i < len(hs); i += mapGroupSize {
			part := hs[i:min(i+mapGroupSize, len(hs))]
			name := p
			key := p
			if len(hs) > mapGroupSize {
				name = part[0] + " … " + part[len(part)-1]
				key = p + "/" + strconv.Itoa(i/mapGroupSize)
			}
			out = append(out, hostGroup{id: siteID + "|" + key, name: name, hosts: part})
		}
	}
	return out
}

func (srv *Server) handleMapServices(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	steps, _ := strconv.Atoi(r.URL.Query().Get("steps"))
	if steps <= 0 || steps > 10 {
		steps = 3
	}
	v, ok := srv.servicesView(r.URL.Query().Get("focus"), steps, now)
	if !ok {
		http.Error(w, "no such service on the map", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// serviceStatus is the worst of a service's parts: a container's verdict,
// a unit's or process's host. A service with no parts is waiting.
func serviceStatus(s mapgraph.Service, hosts map[string]HostView, k8s map[string][2]string) (string, string) {
	if s.Source == "outside" {
		return "waiting", "Outside, not watched"
	}
	if len(s.Parts) == 0 {
		return "waiting", "Nothing found for it yet"
	}
	st, word := "good", "Running"
	for _, p := range s.Parts {
		h, ok := hosts[p.Host]
		ps, pw := cmp.Or(h.Status, "waiting"), h.Word
		if strings.HasPrefix(p.Owner, "k8s:") { // a controller or VM: its record says
			ps, pw = "good", "Running"
			if p.Owner != "k8s:ingress" {
				v, seen := k8s[mapgraph.PartKey(p.Host, p.Owner)]
				ps, pw = cmp.Or(v[0], "waiting"), strings.TrimPrefix(p.Owner, "k8s:")+": "+cmp.Or(v[1], "not seen")
				if !seen || ps == "off" {
					ps = "waiting"
				}
			}
		} else if !ok || h.Silent {
			ps, pw = cmp.Or(h.Status, "waiting"), p.Host+": "+cmp.Or(h.Word, "no reports")
		} else if name, isC := strings.CutPrefix(p.Owner, "container:"); isC {
			ps, pw = "waiting", "Not seen"
			for _, c := range h.rows {
				if c.Name == name {
					ps, pw = c.Verdict.Severity, p.Host+": "+c.Verdict.Word
					if ps == "off" {
						ps = "waiting"
					}
					break
				}
			}
		} else if ps == "good" {
			pw = "Running"
		} else {
			pw = p.Host + ": " + h.Word
		}
		if statusOrder[ps] > statusOrder[st] {
			st, word = ps, pw
		}
	}
	return st, word
}

// declaredSignals is what judges each declared service besides its parts:
// its checks and Cloudflare (topology.yaml), and the units and containers
// it claims, by service id. Those last are mostly its parts already, but a
// unit that failed stops listening and drops off the map: without them a
// service whose one copy is down read "Running" from the copies still up.
func (srv *Server) declaredSignals(now time.Time) map[string][]Signal {
	svcs, ok := srv.serviceStates(now)
	if !ok {
		return nil
	}
	out := map[string][]Signal{}
	for _, sv := range svcs {
		for _, sg := range sv.Signals {
			switch sg.Kind {
			case "check", "cloudflare", "unit", "container":
				out[sv.ID] = append(out[sv.ID], sg)
			}
		}
	}
	return out
}

// withSignals folds a declared service's checks into its parts' status. A
// service with no parts (mail judged only by its checks) takes theirs.
func withSignals(st, word string, noParts bool, sigs []Signal) (string, string) {
	answered := false
	for _, sg := range sigs {
		if sg.Status == "waiting" || sg.Status == "off" {
			continue
		}
		if !answered && noParts {
			st, word = "good", "OK"
		}
		answered = true
		ss := sg.Status
		if ss == "serious" { // the maps colour in four
			ss = "critical"
		}
		if statusOrder[ss] > statusOrder[st] {
			st, word = ss, sg.Name+": "+sg.Word
		}
	}
	return st, word
}

func (srv *Server) servicesView(focus string, steps int, now time.Time) (*MapView, bool) {
	g := srv.mapGraph(now)
	_, hosts := srv.hostStatus(now)
	edges := srv.mapEdges(g, now)
	v := &MapView{Built: g.Built, Focus: focus, Hosts: len(g.Hosts)}
	for _, n := range g.Hosts {
		if n.Mapped {
			v.Mapped++
		}
	}
	svc := map[string]mapgraph.Service{}
	status := map[string]string{}
	words := map[string]string{}
	k8s := srv.k8sParts()
	judged := srv.declaredSignals(now)
	for _, s := range g.Services {
		svc[s.ID] = s
		st, word := serviceStatus(s, hosts, k8s)
		status[s.ID], words[s.ID] = withSignals(st, word, len(s.Parts) == 0, judged[s.ID])
	}
	site := map[string]string{}
	for _, n := range g.Hosts {
		site[n.Name] = n.Site
	}
	siteName := map[string]string{}
	for _, s := range g.Sites {
		siteName[s.ID] = s.Name
	}
	box := func(s mapgraph.Service) MapBox {
		b := MapBox{ID: "svc:" + s.ID, Kind: "service", Name: s.Name, Status: status[s.ID], Sub: words[s.ID],
			Tier: s.Tier, Layer: s.Layer, Ports: s.Internet, Count: len(s.Parts), Open: true}
		var hs []string
		for _, p := range s.Parts {
			hs = appendNew(hs, p.Host)
		}
		// The box says how many; the detail lists the first few. The
		// service's own page (step 5) lists them all.
		b.Detail = map[string]any{"source": s.Source, "kind": s.Kind, "parts": s.Parts[:min(len(s.Parts), mapDetailList)],
			"hosts": hs[:min(len(hs), mapDetailList)], "host_count": len(hs), "ports": s.Ports, "outside": s.Outside, "declared": s.Declared}
		return b
	}
	overview := MapBox{ID: "", Kind: "overview", Name: "Every service"}
	ls := newLineSet()
	// groupOf: where a service goes when there are too many to draw one by
	// one: what was declared together, else the site its first part is in.
	groupOf := func(s mapgraph.Service) (string, string) {
		if c, ok := k8sCluster(s); ok {
			return "group:cluster:" + c, c
		}
		if s.Declared || len(s.Parts) == 0 {
			return "group:declared", "Declared"
		}
		sid := site[s.Parts[0].Host]
		if sid == "" {
			return "group:host:" + s.Parts[0].Host, s.Parts[0].Host
		}
		return "group:" + sid, cmp.Or(siteName[sid], sid)
	}

	switch kind, id, _ := strings.Cut(focus, ":"); kind {
	case "":
		v.Level, v.Title = "overview", "Every service"
		// A Kubernetes cluster is always one box here, opening into its
		// services; the rest are drawn one by one until there are too many.
		few := len(g.Services) <= mapServiceCards
		where := map[string]string{}
		members := map[string][]string{}
		names := map[string]string{}
		var order []string
		layer := map[string]int{}
		for _, s := range g.Services {
			_, inCluster := k8sCluster(s)
			if few && !inCluster {
				v.Boxes = append(v.Boxes, box(s))
				where[s.ID] = "svc:" + s.ID
				continue
			}
			gid, gname := groupOf(s)
			if members[gid] == nil {
				order = append(order, gid)
				layer[gid] = s.Layer
			}
			members[gid] = append(members[gid], s.ID)
			names[gid] = gname
			where[s.ID] = gid
			layer[gid] = min(layer[gid], s.Layer)
		}
		kinds := map[string]string{} // a source's name → what it is, for its box (#37)
		for _, s := range srv.workloadSnapshot() {
			kinds[s.name] = s.kindLabel()
		}
		for _, gid := range order {
			b := fold(gid, "group", names[gid], members[gid], status)
			for i, m := range b.Problems {
				b.Problems[i] = svc[m].Name
			}
			b.Layer = layer[gid]
			if c, ok := strings.CutPrefix(gid, "group:cluster:"); ok {
				b.Sub = cmp.Or(kinds[c], "Kubernetes cluster")
			}
			v.Boxes = append(v.Boxes, b)
		}
		for _, e := range edges {
			ls.add(where[e.From], where[e.To], "needs", true, portList(e.Port), e.Stage)
		}

	case "group":
		gid := "group:" + id
		v.Level = "group"
		v.Crumbs = []MapBox{overview}
		in := map[string]bool{}
		for _, s := range g.Services {
			if g2, _ := groupOf(s); g2 == gid {
				in[s.ID] = true
				_, v.Title = groupOf(s)
				v.Boxes = append(v.Boxes, box(s))
			}
		}
		if len(in) == 0 {
			return nil, false
		}
		others := map[string]MapBox{}
		for _, e := range edges {
			a, b := in[e.From], in[e.To]
			if !a && !b {
				continue
			}
			for _, x := range []string{e.From, e.To} {
				if !in[x] {
					ob := box(svc[x])
					ob.Kind = "elsewhere:service"
					others[ob.ID] = ob
				}
			}
			ls.add("svc:"+e.From, "svc:"+e.To, "needs", true, portList(e.Port), e.Stage)
		}
		v.Boxes = append(v.Boxes, sortedBoxes(others)...)

	case "svc":
		if _, ok := svc[id]; !ok {
			return nil, false
		}
		v.Level, v.Title = "service", svc[id].Name
		v.Crumbs = []MapBox{overview}
		if c, ok := k8sCluster(svc[id]); ok {
			v.Crumbs = append(v.Crumbs, MapBox{ID: "group:cluster:" + c, Kind: "group", Name: c})
		}
		down := map[string][]MapEdge{}
		up := map[string][]MapEdge{}
		for _, e := range edges {
			down[e.From] = append(down[e.From], e)
			up[e.To] = append(up[e.To], e)
		}
		keep := map[string]bool{id: true}
		walk := func(next map[string][]MapEdge, toward func(MapEdge) string) {
			frontier := []string{id}
			for range steps {
				var more []string
				for _, f := range frontier {
					for _, e := range next[f] {
						t := toward(e)
						ls.add("svc:"+e.From, "svc:"+e.To, "needs", true, portList(e.Port), e.Stage)
						if !keep[t] {
							keep[t] = true
							more = append(more, t)
						}
					}
				}
				frontier = more
			}
		}
		walk(down, func(e MapEdge) string { return e.To })
		walk(up, func(e MapEdge) string { return e.From })
		for _, s := range g.Services {
			if keep[s.ID] {
				v.Boxes = append(v.Boxes, box(s))
			}
		}
	default:
		return nil, false
	}
	v.Lines = ls.list()
	slices.SortStableFunc(v.Boxes, func(a, b MapBox) int { return cmp.Compare(a.Layer, b.Layer) })
	v.capped()
	return v, true
}

func portList(p int) []int {
	if p == 0 {
		return nil
	}
	return []int{p}
}

func appendNew(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

// MapFound is a search hit on the maps: where to go to see it.
type MapFound struct {
	Kind  string `json:"kind"` // host, site, service
	Name  string `json:"name"`
	Where string `json:"where,omitempty"` // the site it's in
	Map   string `json:"map"`             // network or services
	Focus string `json:"focus"`
}

// handleMapFind finds hosts, sites and services by name: ?q=, at most 20,
// whole-name matches first.
func (srv *Server) handleMapFind(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []MapFound{}
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"found": out})
		return
	}
	g := srv.mapGraph(time.Now())
	siteName := map[string]string{}
	for _, s := range g.Sites {
		siteName[s.ID] = s.Name
	}
	type hit struct {
		f     MapFound
		exact bool
	}
	var hits []hit
	match := func(names ...string) (bool, bool) {
		for _, n := range names {
			n = strings.ToLower(n)
			if n == q {
				return true, true
			}
			if strings.Contains(n, q) {
				return true, false
			}
		}
		return false, false
	}
	for _, s := range g.Sites {
		if ok, ex := match(s.Name, strings.TrimPrefix(s.ID, "site:")); ok {
			hits = append(hits, hit{MapFound{Kind: "site", Name: s.Name, Map: "network", Focus: s.ID}, ex})
		}
	}
	for _, h := range g.Hosts {
		if ok, ex := match(h.Name); ok {
			hits = append(hits, hit{MapFound{Kind: "host", Name: h.Name, Where: siteName[h.Site], Map: "network", Focus: "host:" + h.Name}, ex})
		}
	}
	for _, s := range g.Services {
		if ok, ex := match(s.Name, s.ID); ok {
			hits = append(hits, hit{MapFound{Kind: "service", Name: s.Name, Map: "services", Focus: "svc:" + s.ID}, ex})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if a.exact != b.exact {
			if a.exact {
				return -1
			}
			return 1
		}
		return cmp.Compare(len(a.f.Name), len(b.f.Name))
	})
	for _, h := range hits[:min(len(hits), 20)] {
		out = append(out, h.f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": out, "more": len(hits) > 20})
}

// k8sCluster is the cluster a service on the map comes from, if it's from
// one: its parts are all that cluster's (docs/maps.md, step 7).
func k8sCluster(s mapgraph.Service) (string, bool) {
	if s.Source != "kubernetes" || len(s.Parts) == 0 {
		return "", false
	}
	return s.Parts[0].Host, true
}
