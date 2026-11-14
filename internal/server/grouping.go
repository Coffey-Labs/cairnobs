package server

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
	"github.com/cairnobs/cairnobs/internal/units"
)

// Root-cause grouping: when something fails and
// something it depends on has failed too, only the deeper failure becomes
// an incident; the others are listed under it as affected.
//
// What depends on what comes from two places (docs/maps.md, Learned
// edges): topology.yaml's needs, and the service map's edges that are
// declared (topology.yaml, Kubernetes, confirmed on the page) or learned
// and steady. A learned edge only seen so far, or fading, never groups,
// and a rejected one is gone from the map. An incident grouped by a
// learned edge says which, so a wrong guess is out in the open, and the
// safety rules stay: the group takes its worst severity, and an alert
// already sent stays on its own.

// GroupedBy is a learned dependency that grouped an incident.
type GroupedBy struct {
	From     string    `json:"from"` // service ids on the map
	To       string    `json:"to"`
	FromName string    `json:"from_name"`
	ToName   string    `json:"to_name"`
	Port     int       `json:"port,omitempty"`
	Since    time.Time `json:"since"` // first seen
}

func (g GroupedBy) String() string {
	p := ""
	if g.Port != 0 {
		p = fmt.Sprintf(" :%d", g.Port)
	}
	return fmt.Sprintf("%s has talked to %s%s since %s", g.FromName, g.ToName, p, g.Since.Format("Jan 2"))
}

// depEdge is one direct need; why is set when it's a learned edge.
type depEdge struct {
	to  string
	why *GroupedBy
}

// deps is what grouping works from: nodes are topology.yaml's services,
// the map's services and hosts.
type deps struct {
	topo  *Topology
	needs map[string][]depEdge
	names map[string]string
	parts map[string]string // mapgraph.PartKey → the map service it's part of
	svc   map[string]bool   // map services, for a link to the service map
	hosts map[string]bool
}

func (d *deps) add(from, to string, why *GroupedBy) {
	if from == "" || to == "" || from == to {
		return
	}
	for i, e := range d.needs[from] {
		if e.to == to {
			if e.why != nil && why == nil { // declared beats learned
				d.needs[from][i].why = nil
			}
			return
		}
	}
	d.needs[from] = append(d.needs[from], depEdge{to, why})
}

// dependencies gathers topology.yaml's and the map's dependencies; nil
// when there are neither.
func (srv *Server) dependencies(now time.Time) *deps {
	t := srv.currentTopology()
	d := &deps{topo: t, needs: map[string][]depEdge{}, names: map[string]string{}, parts: map[string]string{},
		svc: map[string]bool{}, hosts: map[string]bool{}}
	if t != nil {
		for _, s := range t.Services {
			d.names[s.ID] = s.Name
			for _, n := range s.Needs {
				d.add(s.ID, n, nil)
			}
		}
	}
	g := srv.mapGraph(now)
	for _, h := range g.Hosts {
		d.hosts[h.Name] = true
	}
	for _, s := range g.Services {
		d.svc[s.ID] = true
		if d.names[s.ID] == "" {
			d.names[s.ID] = s.Name
		}
		var on []string
		for _, p := range s.Parts {
			d.parts[mapgraph.PartKey(p.Host, p.Owner)] = s.ID
			on = appendNew(on, p.Host)
		}
		if len(on) == 1 && s.Source != "kubernetes" { // all of it on one host: that host going takes it down
			d.add(s.ID, on[0], nil)
		}
	}
	for _, e := range srv.mapEdges(g, now) {
		switch e.Stage {
		case stageDeclared:
			d.add(e.From, e.To, nil)
		case stageSteady:
			why := &GroupedBy{From: e.From, To: e.To, FromName: d.names[e.From], ToName: d.names[e.To], Port: e.Port}
			if e.First != nil {
				why.Since = *e.First
			}
			d.add(e.From, e.To, why)
		}
	}
	// A host that's a VM needs that VM, and the VM its node (vmlink.go):
	// keyed as their conditions are.
	byHost, _ := srv.vmLinks()
	for h, v := range byHost {
		d.add(h, v.key, nil)
		d.names[v.key] = "VM " + where(v.Name, v.Namespace) + " (" + v.place() + ")"
		if v.Node != "" {
			n := "node/" + v.Source + "/" + v.Node
			d.add(v.key, n, nil)
			d.names[n] = "node " + v.Node + " (" + v.Source + ")"
		}
	}
	// A libvirt VM needs the host it runs on: that host silent explains it,
	// and through it the guest's own agent going quiet.
	for _, h := range srv.store.HostVMs() {
		for _, r := range h.vms.List {
			k := "vm/" + h.name + "//" + r.Name
			d.add(k, h.name, nil)
			d.names[k] = "VM " + r.Name + " (" + h.name + ")"
		}
	}
	if t == nil && len(d.needs) == 0 {
		return nil
	}
	return d
}

// nodeOf says which node a condition belongs to: a service, a host, or ""
// when nothing covers it (it then stands alone).
func (d *deps) nodeOf(c Condition) string {
	n := ""
	if d.topo != nil {
		n = nodeOf(d.topo, c)
	}
	parts := strings.SplitN(c.Key, "/", 3)
	switch parts[0] {
	case "silent":
		return c.Host
	case "vm", "node": // a cluster's VM or node, as dependencies keys them
		return c.Key
	case "unit", "container":
		if len(parts) < 3 {
			return n
		}
		host := parts[1]
		if n != "" && n != host {
			return n // topology.yaml placed it
		}
		if s := d.parts[mapgraph.PartKey(host, parts[0]+":"+parts[2])]; s != "" {
			return s
		}
		return host
	case "workload": // workload/<cluster>/<ns>/<controller>: its service on the map
		if len(parts) == 3 {
			if s := d.parts[mapgraph.PartKey(parts[1], "k8s:"+parts[2])]; s != "" {
				return s
			}
		}
	}
	return n
}

// mapHref is where a node is on the maps, or "".
func (d *deps) mapHref(node string) string {
	switch {
	case d.svc[node]:
		return "/map/services?focus=svc:" + node
	case d.hosts[node]:
		return "/map/network?focus=host:" + node
	}
	return ""
}

// path is a shortest way from one node to another through what each
// needs, as the edges taken; nil when there's none.
func (d *deps) path(from, to string) []depEdge {
	type step struct {
		prev string
		e    depEdge
	}
	seen := map[string]step{from: {}}
	queue := []string{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n == to {
			var out []depEdge
			for n != from {
				s := seen[n]
				out = append(out, s.e)
				n = s.prev
			}
			slices.Reverse(out)
			return out
		}
		for _, e := range d.needs[n] {
			if _, ok := seen[e.to]; !ok {
				seen[e.to] = step{n, e}
				queue = append(queue, e.to)
			}
		}
	}
	return nil
}

// reach is everything a node depends on, all the way down.
func (d *deps) reach(n string) map[string]bool {
	seen := map[string]bool{}
	todo := []string{n}
	for len(todo) > 0 {
		x := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		for _, e := range d.needs[x] {
			if !seen[e.to] {
				seen[e.to] = true
				todo = append(todo, e.to)
			}
		}
	}
	return seen
}

// nodeOf says which topology.yaml node a condition belongs to: a service,
// a host, or "" when the file doesn't cover it.
func nodeOf(t *Topology, c Condition) string {
	parts := strings.SplitN(c.Key, "/", 3)
	kind := parts[0]
	switch kind {
	case "silent":
		return c.Host // a host that's gone explains everything on it
	case "disk", "memory", "containers-unreadable":
		return "" // trouble on a host, but not why its services fail: stands alone
	case "unit", "container":
		if len(parts) < 3 {
			return ""
		}
		host, name := parts[1], parts[2]
		for _, s := range t.Services {
			pats := s.Containers
			if kind == "unit" {
				pats = nil
				for _, u := range s.Units {
					h, n, _ := strings.Cut(u, "/")
					pats = append(pats, h+"/"+units.Name(n))
				}
			}
			for _, p := range pats {
				if hostMatch(p, host, name) {
					return s.ID
				}
			}
		}
		return host // not in any service: at least it's on that host
	case "check", "cert":
		id := strings.TrimPrefix(c.Key, kind+"/")
		for _, s := range t.Services {
			if slices.Contains(s.Checks, id) {
				return s.ID
			}
		}
	case "drift": // drift/<container>: the service that runs it
		name := strings.TrimPrefix(c.Key, "drift/")
		for _, s := range t.Services {
			for _, p := range s.Containers {
				if _, n, _ := strings.Cut(p, "/"); func() bool { ok, _ := path.Match(n, name); return ok }() {
					return s.ID
				}
			}
		}
		return ""
	case "dl": // data layer: dl/<service>/...; the service map ids match (patroni, etcd, haproxy, nats, garage)
		if len(parts) > 1 {
			for _, s := range t.Services {
				if s.ID == parts[1] {
					return s.ID
				}
			}
		}
		return c.Host
	case "mail": // the monitor account's view of mail: the service map's mail node
		for _, s := range t.Services {
			if s.ID == "mail" {
				return s.ID
			}
		}
		return ""
	case "akamai":
		if len(parts) > 1 && parts[1] == "server" { // GTM can't reach a host: that's the host's trouble
			return c.Host
		}
	case "cloudflare":
		if len(parts) > 1 && parts[1] == "origin" { // Cloudflare can't reach a host: that's the host's trouble
			return c.Host
		}
		for _, s := range t.Services {
			if s.Cloudflare {
				return s.ID
			}
		}
	}
	return ""
}

// groupConditions folds conditions caused by a deeper failure into the
// condition for that failure. firing says whether a key already has an
// incident that people were told about: such a condition stays on its
// own, so it neither vanishes nor reads as "resolved" because its cause
// was spotted later.
func (srv *Server) groupConditions(conds []Condition, firing func(key string) bool) []Condition {
	if len(conds) == 0 {
		return conds
	}
	d := srv.dependencies(time.Now())
	if d == nil {
		return conds
	}
	node := make([]string, len(conds))
	failing := map[string]bool{}
	for i, c := range conds {
		node[i] = d.nodeOf(c)
		if node[i] != "" {
			failing[node[i]] = true
		}
		if conds[i].Map == "" {
			conds[i].Map = d.mapHref(node[i])
		}
	}
	if len(conds) < 2 {
		return conds
	}
	// What each failing node depends on, worked out only for those.
	below := map[string]map[string]bool{}
	for n := range failing {
		below[n] = d.reach(n)
	}
	// A failing node is a root when nothing it needs is failing.
	isRoot := func(n string) bool {
		for m := range below[n] {
			if failing[m] {
				return false
			}
		}
		return true
	}
	// The condition that speaks for each root node: the most severe, then
	// the first by key, so the choice is stable from one evaluation to the next.
	order := make([]int, len(conds))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ca, cb := conds[order[a]], conds[order[b]]
		if ca.Severity != cb.Severity {
			return ca.Severity == "critical"
		}
		return ca.Key < cb.Key
	})
	speaker := map[string]int{}
	for _, i := range order {
		if n := node[i]; n != "" && isRoot(n) {
			if _, ok := speaker[n]; !ok {
				speaker[n] = i
			}
		}
	}

	type group struct {
		affected []string // node names, in order of appearance
		held     int
		severity string
		why      []GroupedBy
	}
	groups := map[int]*group{}
	keep := make([]bool, len(conds))
	for _, i := range order {
		c, n := conds[i], node[i]
		keep[i] = true
		if n == "" || isRoot(n) || firing(c.Key) {
			continue
		}
		// Hand it to the first root (in speaker order) that it depends on.
		root := -1
		for _, j := range order {
			if m := node[j]; m != "" && speaker[m] == j && below[n][m] {
				root = j
				break
			}
		}
		if root < 0 {
			continue
		}
		keep[i] = false
		g := groups[root]
		if g == nil {
			g = &group{severity: conds[root].Severity}
			groups[root] = g
		}
		g.held++
		label := cmp.Or(d.names[n], n)
		if !slices.Contains(g.affected, label) {
			g.affected = append(g.affected, label)
		}
		if c.Severity == "critical" {
			g.severity = "critical"
		}
		for _, e := range d.path(n, node[root]) {
			if e.why != nil && !slices.Contains(g.why, *e.why) {
				g.why = append(g.why, *e.why)
			}
		}
	}
	out := make([]Condition, 0, len(conds))
	for i, c := range conds {
		if !keep[i] {
			continue
		}
		if g := groups[i]; g != nil {
			c.Severity = g.severity
			c.Affected = strings.Join(g.affected, ", ")
			c.Grouped = g.why
			d := strings.TrimSpace(c.Detail)
			if d != "" && !strings.HasSuffix(d, ".") {
				d += "."
			}
			d += fmt.Sprintf(" Also affected: %s (%s held back).", c.Affected, plural(g.held, "alert", "alerts"))
			if len(g.why) > 0 {
				var ws []string
				for _, w := range g.why[:min(len(g.why), 3)] {
					ws = append(ws, w.String())
				}
				more := ""
				if len(g.why) > 3 {
					more = fmt.Sprintf(" (and %d more)", len(g.why)-3)
				}
				d += " Grouped because " + strings.Join(ws, "; ") + more + "."
			}
			c.Detail = strings.TrimSpace(d)
		}
		out = append(out, c)
	}
	return out
}

// needsClosure is every topology.yaml node each node depends on, all the
// way down, for the hand-drawn service map.
func needsClosure(t *Topology) map[string]map[string]bool {
	d := &deps{needs: map[string][]depEdge{}}
	for _, s := range t.Services {
		for _, n := range s.Needs {
			d.add(s.ID, n, nil)
		}
	}
	out := map[string]map[string]bool{}
	for _, s := range t.Services {
		out[s.ID] = d.reach(s.ID)
	}
	return out
}
