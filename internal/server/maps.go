package server

// The maps, drawn from discovery (docs/maps.md). Each copy builds the graph
// from the hosts' latest map samples in its store, at most every
// mapRebuild. The evaluator records the learned edges it sees, once an hour
// per edge, in map_edges; every copy reads them back to tell an edge seen
// once from a steady or a fading one.

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	mapRebuild   = 10 * time.Second // how stale a copy's graph may get
	mapEdgesRead = time.Minute      // how often a copy rereads map_edges
	mapRecord    = time.Minute      // how often the evaluator records edges
	edgeFading   = 14 * 24 * time.Hour
	edgeDropped  = 30 * 24 * time.Hour
)

// Edge stages, as the maps draw them.
const (
	stageDeclared = "declared"
	stageSeen     = "seen"
	stageSteady   = "steady"
	stageFading   = "fading"
)

type mapState struct {
	mu      sync.Mutex
	graph   *mapgraph.Graph
	edges   map[state.EdgeKey]state.EdgeRow // learned edges as stored
	edgesAt time.Time

	recorded map[state.EdgeKey]int64          // evaluator: the hour each edge was last recorded
	mem      map[state.EdgeKey]*state.EdgeRow // without a database

	fixes *MapFixes  // corrections made on the page (mapfixes.go)
	fixMu sync.Mutex // held while a correction is checked and saved

	svcFirst    map[string]time.Time // when each service was first seen, as stored
	svcAt       time.Time
	svcRecorded map[string]int64 // evaluator: the hour each service was last recorded
	svcMem      map[string]*state.ServiceRow
}

// mapHosts is every host's latest view of itself, for the graph.
// mapClusters is what the maps take from each cluster: its controllers,
// VMs, Services, Ingresses and Routes.
func (srv *Server) mapClusters() []mapgraph.Cluster {
	ws := &srv.wl
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := make([]mapgraph.Cluster, 0, len(ws.src))
	for name, s := range ws.src {
		c := mapgraph.Cluster{Name: name}
		for _, r := range s.recs {
			switch r.Kind {
			case report.KindController, report.KindVM, report.KindService, report.KindIngress:
				c.Records = append(c.Records, r.Workload)
			}
		}
		if len(c.Records) > 0 {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b mapgraph.Cluster) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// k8sParts is each Kubernetes part's state, for the service map:
// PartKey(cluster, "k8s:ns/deployment/web") → severity and word.
func (srv *Server) k8sParts() map[string][2]string {
	ws := &srv.wl
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	out := map[string][2]string{}
	for name, s := range ws.src {
		for _, r := range s.recs {
			switch r.Kind {
			case report.KindController:
				out[mapgraph.PartKey(name, mapgraph.K8sOwner(r.Namespace, r.Name))] = [2]string{stateSeverity(r.State), r.Word}
			case report.KindVM:
				out[mapgraph.PartKey(name, mapgraph.K8sOwner(r.Namespace, "vm/"+r.Name))] = [2]string{stateSeverity(r.State), r.Word}
			}
		}
	}
	return out
}

func (srv *Server) mapHosts() []mapgraph.Host {
	snap := srv.store.Snapshot()
	out := make([]mapgraph.Host, 0, len(snap))
	for _, h := range snap {
		mh := mapgraph.Host{Name: h.name, Map: h.mp, Workstation: srv.cfg.workstation(h.name)}
		if h.last.Containers != nil {
			mh.Containers = h.last.Containers.List
		}
		out = append(out, mh)
	}
	return out
}

// mapCorrections is topology.yaml read as corrections to what's found,
// with the page's on top.
func (srv *Server) mapCorrections() mapgraph.Corrections {
	var c mapgraph.Corrections
	srv.mapFixesNow().apply(&c)
	t := srv.currentTopology()
	if t == nil {
		return c
	}
	c.SiteName = t.Network.Site
	c.Roles = map[string]string{}
	for _, h := range t.Network.Hosts {
		c.SiteHosts = append(c.SiteHosts, h.Name)
		if h.Roles != "" {
			c.Roles[h.Name] = h.Roles
		}
	}
	for _, r := range t.Network.Remote {
		if r.Roles != "" {
			c.Roles[r.Name] = r.Roles
		}
	}
	for _, s := range t.Services {
		c.Services = append(c.Services, mapgraph.Declared{ID: s.ID, Name: s.Name, Tier: s.Tier, Needs: s.Needs,
			Containers: s.Containers, Units: s.Units})
	}
	return c
}

// mapGraph is this copy's graph, rebuilt when it's older than mapRebuild.
func (srv *Server) mapGraph(now time.Time) *mapgraph.Graph {
	srv.maps.mu.Lock()
	g := srv.maps.graph
	srv.maps.mu.Unlock()
	if g != nil && now.Sub(g.Built) < mapRebuild && !now.Before(g.Built) {
		return g
	}
	g = mapgraph.Build(srv.mapHosts(), srv.mapCorrections(), now, srv.mapClusters()...)
	srv.maps.mu.Lock()
	srv.maps.graph = g
	srv.maps.mu.Unlock()
	return g
}

// learnedEdges is what map_edges holds, reread when older than mapEdgesRead.
func (srv *Server) learnedEdges(now time.Time) map[state.EdgeKey]state.EdgeRow {
	srv.maps.mu.Lock()
	defer srv.maps.mu.Unlock()
	if srv.db == nil {
		out := make(map[state.EdgeKey]state.EdgeRow, len(srv.maps.mem))
		for k, r := range srv.maps.mem {
			out[k] = *r
		}
		return out
	}
	if srv.maps.edges != nil && now.Sub(srv.maps.edgesAt) < mapEdgesRead {
		return srv.maps.edges
	}
	rows, err := srv.db.Edges()
	if err != nil {
		log.Printf("maps: can't read the learned edges: %v", err)
		if srv.maps.edges == nil {
			srv.maps.edges = map[state.EdgeKey]state.EdgeRow{}
		}
		return srv.maps.edges
	}
	m := make(map[state.EdgeKey]state.EdgeRow, len(rows))
	for _, r := range rows {
		m[r.EdgeKey] = r
	}
	srv.maps.edges, srv.maps.edgesAt = m, now
	return m
}

// recordEdges is the evaluator's: every mapRecord, note each learned edge
// it sees, at most once an hour each, and once a day drop the ones not
// seen for edgeDropped.
func (srv *Server) recordEdges(ctx context.Context) {
	t := time.NewTicker(mapRecord)
	defer t.Stop()
	var purged time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			srv.recordEdgesOnce(now)
			if srv.db != nil && now.Sub(purged) > 24*time.Hour {
				if n, err := srv.db.PurgeEdges(now.Add(-edgeDropped)); err != nil {
					log.Printf("maps: can't drop old edges: %v", err)
				} else if n > 0 {
					log.Printf("maps: dropped %d edges not seen for %s", n, edgeDropped)
				}
				if _, err := srv.db.PurgeServices(now.Add(-edgeDropped)); err != nil {
					log.Printf("maps: can't drop old services: %v", err)
				}
				purged = now
			}
		}
	}
}

func (srv *Server) recordEdgesOnce(now time.Time) {
	g := srv.mapGraph(now)
	srv.recordServices(g, now)
	hour := now.Unix() / 3600
	srv.maps.mu.Lock()
	if srv.maps.recorded == nil {
		srv.maps.recorded = map[state.EdgeKey]int64{}
	}
	var due []state.EdgeKey
	for _, e := range g.Edges {
		if e.Source != "learned" {
			continue
		}
		k := state.EdgeKey{Src: e.From, Dst: e.To, Port: e.Port}
		if srv.maps.recorded[k] != hour {
			due = append(due, k)
		}
	}
	if srv.db == nil {
		if srv.maps.mem == nil {
			srv.maps.mem = map[state.EdgeKey]*state.EdgeRow{}
		}
		for _, k := range due {
			r := srv.maps.mem[k]
			if r == nil {
				r = &state.EdgeRow{EdgeKey: k}
				srv.maps.mem[k] = r
			}
			r.Seen(now)
			srv.maps.recorded[k] = hour
		}
		for k, r := range srv.maps.mem {
			if now.Sub(r.Last) > edgeDropped {
				delete(srv.maps.mem, k)
			}
		}
		srv.maps.mu.Unlock()
		return
	}
	srv.maps.mu.Unlock()
	if len(due) == 0 {
		return
	}
	if err := srv.db.SeenEdges(due, now); err != nil {
		log.Printf("maps: can't record %d edges: %v", len(due), err)
		return
	}
	srv.maps.mu.Lock()
	for _, k := range due {
		srv.maps.recorded[k] = hour
	}
	srv.maps.edgesAt = time.Time{} // reread on the next look
	srv.maps.mu.Unlock()
}

// edgeStage says how far to believe a learned edge.
func edgeStage(r state.EdgeRow, ok bool, now time.Time) string {
	switch {
	case !ok:
		return stageSeen // seen now, not recorded yet
	case now.Sub(r.Last) > edgeFading:
		return stageFading
	case !r.Steady.IsZero():
		return stageSteady
	}
	return stageSeen
}

// MapEdge is an edge with its stage, as the maps get it.
type MapEdge struct {
	mapgraph.Edge
	Stage string     `json:"stage"`
	First *time.Time `json:"first_seen,omitempty"`
	Last  *time.Time `json:"last_seen,omitempty"`
}

// mapEdges is the graph's edges with their stages, plus learned edges not
// seen now but within edgeDropped, between services that still exist.
func (srv *Server) mapEdges(g *mapgraph.Graph, now time.Time) []MapEdge {
	rows := srv.learnedEdges(now)
	cor := srv.mapCorrections()
	have := map[string]bool{}
	for _, s := range g.Services {
		have[s.ID] = true
	}
	seen := map[state.EdgeKey]bool{}
	out := make([]MapEdge, 0, len(g.Edges))
	for _, e := range g.Edges {
		me := MapEdge{Edge: e, Stage: stageDeclared}
		k := state.EdgeKey{Src: e.From, Dst: e.To, Port: e.Port}
		seen[k] = true // a confirmed edge is declared now, not drawn twice
		if e.Source == "learned" {
			r, ok := rows[k]
			me.Stage = edgeStage(r, ok, now)
			if ok {
				me.First, me.Last = &r.First, &r.Last
			}
		}
		out = append(out, me)
	}
	for k, r := range rows {
		if seen[k] || !have[k.Src] || !have[k.Dst] || now.Sub(r.Last) > edgeDropped ||
			cor.Rejects(mapgraph.Edge{From: k.Src, To: k.Dst, Port: k.Port}) {
			continue
		}
		out = append(out, MapEdge{Edge: mapgraph.Edge{From: k.Src, To: k.Dst, Port: k.Port, Source: "learned"},
			Stage: edgeStage(r, true, now), First: &r.First, Last: &r.Last})
	}
	return out
}
