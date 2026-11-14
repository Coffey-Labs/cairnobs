package server

// Corrections to the maps, made on the page by admins (docs/maps.md,
// Corrections): rename, set a tier, hide, merge, move a part, confirm or
// reject a learned edge, add what can't be seen. Kept in the database as
// the `maps` section, like the other settings edited in the UI, and put on
// top of topology.yaml's.
//
//	GET  /api/v1/map/corrections   what's been corrected, each with its undo
//	POST /api/v1/map/corrections   one change (admins)
//	GET  /api/v1/map/found?since=  services and edges first seen since then

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapgraph"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	sectionMaps    = "maps"
	mapFixesMax    = 5000 // corrections in all; past that, topology.yaml is the place
	mapFoundMax    = 50   // of each, services and edges, in the found list
	mapFoundOldest = 7 * 24 * time.Hour
)

// MapFixes is the `maps` section.
type MapFixes struct {
	Names     map[string]string  `yaml:"names,omitempty" json:"names,omitempty"`
	Tiers     map[string]string  `yaml:"tiers,omitempty" json:"tiers,omitempty"`
	Hidden    []string           `yaml:"hidden,omitempty" json:"hidden,omitempty"`
	Merged    map[string]string  `yaml:"merged,omitempty" json:"merged,omitempty"`
	Moved     map[string]string  `yaml:"moved,omitempty" json:"moved,omitempty"`
	Confirmed []mapgraph.EdgeRef `yaml:"confirmed,omitempty" json:"confirmed,omitempty"`
	Rejected  []mapgraph.EdgeRef `yaml:"rejected,omitempty" json:"rejected,omitempty"`
	Outside   []mapgraph.Outside `yaml:"outside,omitempty" json:"outside,omitempty"`
}

func (f *MapFixes) size() int {
	return len(f.Names) + len(f.Tiers) + len(f.Hidden) + len(f.Merged) + len(f.Moved) +
		len(f.Confirmed) + len(f.Rejected) + len(f.Outside)
}

func (f *MapFixes) clone() *MapFixes {
	b, _ := json.Marshal(f)
	var c MapFixes
	_ = json.Unmarshal(b, &c)
	return &c
}

// apply puts the page's corrections into c.
func (f *MapFixes) apply(c *mapgraph.Corrections) {
	if f == nil {
		return
	}
	c.Names, c.Tiers, c.Merged, c.Moved = f.Names, f.Tiers, f.Merged, f.Moved
	c.Hidden = map[string]bool{}
	for _, h := range f.Hidden {
		c.Hidden[h] = true
	}
	c.Confirmed, c.Rejected, c.Outside = f.Confirmed, f.Rejected, f.Outside
}

// mapFixesNow is the page's corrections in force; never nil.
func (srv *Server) mapFixesNow() *MapFixes {
	srv.maps.mu.Lock()
	defer srv.maps.mu.Unlock()
	if srv.maps.fixes == nil {
		return &MapFixes{}
	}
	return srv.maps.fixes
}

// loadMapFixes reads the `maps` section at start, or as another copy saved it.
func (srv *Server) loadMapFixes() error {
	if srv.db == nil {
		return nil
	}
	s, ok, err := srv.db.Setting(sectionMaps)
	if err != nil || !ok {
		return err
	}
	var f MapFixes
	if err := decodeStrict([]byte(s.YAML), &f); err != nil {
		return err
	}
	srv.maps.mu.Lock()
	srv.maps.fixes, srv.maps.graph = &f, nil
	srv.maps.mu.Unlock()
	return nil
}

// MapFixReq is one change. Op says which; the other fields are its own.
//
//	name     id, name ("" puts the found name back)
//	tier     id, tier (edge, apps, data; "" works it out again)
//	hide     id (a service, or host:<name>); show undoes it
//	merge    id, into ("" splits it out again)
//	move     host, owner, into (a service id) or name (a new service); into "" puts it back
//	edge     from, to, port, verdict (confirm, reject, "" forgets either)
//	outside  outside (id empty for a new one); drop-outside id
type MapFixReq struct {
	Op      string            `json:"op"`
	ID      string            `json:"id,omitempty"`
	Name    string            `json:"name,omitempty"`
	Tier    string            `json:"tier,omitempty"`
	Into    string            `json:"into,omitempty"`
	Host    string            `json:"host,omitempty"`
	Owner   string            `json:"owner,omitempty"`
	From    string            `json:"from,omitempty"`
	To      string            `json:"to,omitempty"`
	Port    int               `json:"port,omitempty"`
	Verdict string            `json:"verdict,omitempty"`
	Outside *mapgraph.Outside `json:"outside,omitempty"`
}

// MapFixItem is one correction as the page lists it, with what undoes it.
type MapFixItem struct {
	Kind string    `json:"kind"`
	Text string    `json:"text"`
	Undo MapFixReq `json:"undo"`
}

var slugOK = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugOK.ReplaceAllString(strings.ToLower(s), "-"), "-")
	return s[:min(len(s), 40)]
}

// change applies req to f, checked against the graph as it is with f in
// force. It says what's wrong, for the person who asked.
func (srv *Server) change(f *MapFixes, req MapFixReq, g *mapgraph.Graph) error {
	svc := map[string]mapgraph.Service{}
	for _, s := range g.Services {
		svc[s.ID] = s
	}
	site := map[string]bool{}
	for _, s := range g.Sites {
		site[s.ID] = true
	}
	known := func(id string) bool { _, ok := svc[id]; return ok }
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > 80 {
		return fmt.Errorf("names can be at most 80 characters")
	}
	setMap := func(m *map[string]string, k, v string) {
		if *m == nil {
			*m = map[string]string{}
		}
		if v == "" {
			delete(*m, k)
		} else {
			(*m)[k] = v
		}
	}
	switch req.Op {
	case "name":
		if req.Name != "" && !known(req.ID) && !site[req.ID] {
			return fmt.Errorf("there's no service or site %q on the map", req.ID)
		}
		setMap(&f.Names, req.ID, req.Name)
	case "tier":
		if !known(req.ID) && f.Tiers[req.ID] == "" {
			return fmt.Errorf("there's no service %q on the map", req.ID)
		}
		if req.Tier != "" && req.Tier != "edge" && req.Tier != "apps" && req.Tier != "data" {
			return fmt.Errorf("a tier is edge, apps or data")
		}
		setMap(&f.Tiers, req.ID, req.Tier)
	case "hide":
		host, isHost := strings.CutPrefix(req.ID, "host:")
		if isHost && !srv.knownAgent(host) || !isHost && !known(req.ID) {
			return fmt.Errorf("there's nothing called %q on the maps", req.ID)
		}
		if !slices.Contains(f.Hidden, req.ID) {
			f.Hidden = append(f.Hidden, req.ID)
		}
	case "show":
		f.Hidden = slices.DeleteFunc(f.Hidden, func(h string) bool { return h == req.ID })
	case "merge":
		if req.Into != "" {
			if !known(req.ID) || !known(req.Into) || req.ID == req.Into {
				return fmt.Errorf("merging needs two services on the map")
			}
			c := mapgraph.Corrections{Merged: f.Merged}
			if c.MergedInto(req.Into) == req.ID {
				return fmt.Errorf("%s is already merged into %s", svc[req.Into].Name, svc[req.ID].Name)
			}
		}
		setMap(&f.Merged, req.ID, req.Into)
	case "move":
		if req.Host == "" || req.Owner == "" {
			return fmt.Errorf("moving a part needs its host and owner")
		}
		into := req.Into
		if into == "" && req.Name != "" {
			if into = "page:" + slug(req.Name); into == "page:" {
				return fmt.Errorf("that name has no letters or digits in it")
			}
			if !known(into) {
				setMap(&f.Names, into, req.Name)
			}
		} else if into != "" && !known(into) {
			return fmt.Errorf("there's no service %q on the map", into)
		}
		setMap(&f.Moved, mapgraph.PartKey(req.Host, req.Owner), into)
		// A service made by moving parts goes, name and all, with its last part.
		for id := range f.Names {
			if strings.HasPrefix(id, "page:") && !slices.Contains(slices.Collect(maps.Values(f.Moved)), id) {
				delete(f.Names, id)
			}
		}
	case "edge":
		ref := mapgraph.EdgeRef{From: req.From, To: req.To, Port: req.Port}
		if req.Verdict != "" && (!known(req.From) || !known(req.To) || req.From == req.To) {
			return fmt.Errorf("that edge isn't between two services on the map")
		}
		drop := func(l []mapgraph.EdgeRef) []mapgraph.EdgeRef {
			return slices.DeleteFunc(l, func(r mapgraph.EdgeRef) bool { return r == ref })
		}
		f.Confirmed, f.Rejected = drop(f.Confirmed), drop(f.Rejected)
		switch req.Verdict {
		case "confirm":
			f.Confirmed = append(f.Confirmed, ref)
		case "reject":
			f.Rejected = append(f.Rejected, ref)
		case "":
		default:
			return fmt.Errorf("a verdict is confirm or reject")
		}
	case "outside":
		o := req.Outside
		if o == nil || strings.TrimSpace(o.Name) == "" {
			return fmt.Errorf("an outside service needs a name")
		}
		o.Name, o.Note = strings.TrimSpace(o.Name), strings.TrimSpace(o.Note)
		if len(o.Name) > 80 || len(o.Note) > 300 || len(o.Addrs) > 20 || len(o.NeededBy) > 50 {
			return fmt.Errorf("too much: a name of 80 characters, a note of 300, 20 addresses and 50 services at most")
		}
		for _, a := range o.Addrs {
			if !mapgraph.ValidAddr(a) {
				return fmt.Errorf("%q isn't an address, address:port or prefix", a)
			}
		}
		for _, n := range o.NeededBy {
			if !known(n) {
				return fmt.Errorf("there's no service %q on the map", n)
			}
		}
		if o.ID == "" {
			base := "outside:" + slug(o.Name)
			if base == "outside:" {
				return fmt.Errorf("that name has no letters or digits in it")
			}
			o.ID = base
			for i := 2; slices.ContainsFunc(f.Outside, func(x mapgraph.Outside) bool { return x.ID == o.ID }) || (known(o.ID) && svc[o.ID].Source != "outside"); i++ {
				o.ID = base + "-" + strconv.Itoa(i)
			}
		} else if !strings.HasPrefix(o.ID, "outside:") {
			return fmt.Errorf("an outside service's id starts with outside:")
		}
		if i := slices.IndexFunc(f.Outside, func(x mapgraph.Outside) bool { return x.ID == o.ID }); i >= 0 {
			f.Outside[i] = *o
		} else {
			f.Outside = append(f.Outside, *o)
		}
	case "drop-outside":
		f.Outside = slices.DeleteFunc(f.Outside, func(x mapgraph.Outside) bool { return x.ID == req.ID })
	default:
		return fmt.Errorf("unknown change %q", req.Op)
	}
	if f.size() > mapFixesMax {
		return fmt.Errorf("that's more than %d corrections; put the rest in topology.yaml", mapFixesMax)
	}
	return nil
}

func (srv *Server) handleMapCorrectionPost(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.mapFixesSource(), "map corrections", "") {
		return
	}
	var req MapFixReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That isn't a change Cairn OBS understands."})
		return
	}
	srv.maps.fixMu.Lock()
	defer srv.maps.fixMu.Unlock()
	f := srv.mapFixesNow().clone()
	if err := srv.change(f, req, srv.mapGraph(time.Now())); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := srv.saveSection(sectionMaps, f, srv.who(r)); err != nil {
		log.Printf("maps: can't save a correction: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The correction couldn't be saved; see the server's log."})
		return
	}
	srv.maps.mu.Lock()
	srv.maps.fixes, srv.maps.graph = f, nil
	srv.maps.mu.Unlock()
	srv.audit(srv.who(r), "corrected the maps", describeFix(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (srv *Server) mapFixesSource() string {
	if srv.db != nil {
		return sourceUI
	}
	return ""
}

func describeFix(r MapFixReq) string {
	switch r.Op {
	case "edge":
		return fmt.Sprintf("edge %s → %s :%d %s", r.From, r.To, r.Port, cmp.Or(r.Verdict, "forgotten"))
	case "move":
		return fmt.Sprintf("move %s %s → %s", r.Host, r.Owner, cmp.Or(r.Into, r.Name, "back"))
	case "outside":
		if r.Outside != nil {
			return "outside service " + r.Outside.Name
		}
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s %s%s%s", r.Op, r.ID, r.Name, r.Tier, r.Into))
}

// handleMapCorrections lists what's been corrected, by people on the page
// and in topology.yaml, with how to undo each page correction.
func (srv *Server) handleMapCorrections(w http.ResponseWriter, r *http.Request) {
	f := srv.mapFixesNow()
	g := srv.mapGraph(time.Now())
	name := map[string]string{}
	for _, s := range g.Services {
		name[s.ID] = s.Name
	}
	for _, s := range g.Sites {
		name[s.ID] = s.Name
	}
	// A cluster's service says which cluster: two clusters often run an
	// app of the same name.
	inCluster := func(id, n string) string {
		if rest, ok := strings.CutPrefix(id, "k8s:"); ok {
			if c, _, ok := strings.Cut(rest, "/"); ok {
				return n + " (" + c + ")"
			}
		}
		return n
	}
	nm := func(id string) string { return inCluster(id, cmp.Or(name[id], mapgraph.LabelOf(id))) }
	label := func(id string) string { return inCluster(id, mapgraph.LabelOf(id)) }
	var items []MapFixItem
	for _, id := range sortedKeys(f.Names) {
		items = append(items, MapFixItem{"name", fmt.Sprintf("%s is called %q", label(id), f.Names[id]), MapFixReq{Op: "name", ID: id}})
	}
	for _, id := range sortedKeys(f.Tiers) {
		items = append(items, MapFixItem{"tier", fmt.Sprintf("%s is in the %s tier", nm(id), f.Tiers[id]), MapFixReq{Op: "tier", ID: id}})
	}
	for _, id := range f.Hidden {
		items = append(items, MapFixItem{"hide", fmt.Sprintf("%s is hidden", label(id)), MapFixReq{Op: "show", ID: id}})
	}
	for _, id := range sortedKeys(f.Merged) {
		items = append(items, MapFixItem{"merge", fmt.Sprintf("%s is part of %s", label(id), nm(f.Merged[id])), MapFixReq{Op: "merge", ID: id}})
	}
	for _, k := range sortedKeys(f.Moved) {
		host, owner, _ := strings.Cut(k, " ")
		items = append(items, MapFixItem{"move", fmt.Sprintf("%s on %s belongs to %s", owner, host, nm(f.Moved[k])), MapFixReq{Op: "move", Host: host, Owner: owner}})
	}
	port := func(p int) string {
		if p == 0 {
			return ""
		}
		return fmt.Sprintf(" on :%d", p)
	}
	for _, e := range f.Confirmed {
		items = append(items, MapFixItem{"confirm", fmt.Sprintf("%s needs %s%s", nm(e.From), nm(e.To), port(e.Port)), MapFixReq{Op: "edge", From: e.From, To: e.To, Port: e.Port}})
	}
	for _, e := range f.Rejected {
		items = append(items, MapFixItem{"reject", fmt.Sprintf("%s doesn't need %s%s", nm(e.From), nm(e.To), port(e.Port)), MapFixReq{Op: "edge", From: e.From, To: e.To, Port: e.Port}})
	}
	for _, o := range f.Outside {
		items = append(items, MapFixItem{"outside", fmt.Sprintf("%s, outside (%s)", o.Name, cmp.Or(strings.Join(o.Addrs, ", "), "no addresses")), MapFixReq{Op: "drop-outside", ID: o.ID}})
	}
	var updated time.Time
	by := ""
	if srv.db != nil {
		if s, ok, err := srv.db.Setting(sectionMaps); err == nil && ok {
			updated, by = s.Updated, s.UpdatedBy
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"editable": srv.mapFixesSource() == sourceUI, "fixes": f, "items": items,
		"topology": srv.currentTopology() != nil, "updated": updated, "updated_by": by,
	})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// MapFoundService is a service first seen recently.
type MapFoundService struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Kind  string    `json:"kind,omitempty"`
	Hosts []string  `json:"hosts"`
	First time.Time `json:"first_seen"`
}

// MapFoundEdge is a learned edge first seen recently.
type MapFoundEdge struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	FromName string    `json:"from_name"`
	ToName   string    `json:"to_name"`
	Port     int       `json:"port"`
	Stage    string    `json:"stage"`
	First    time.Time `json:"first_seen"`
}

// handleMapFound lists services and learned edges first seen since
// ?since= (Unix milliseconds; at most a week back), newest first, leaving
// out what's been confirmed, rejected or hidden since, and a Kubernetes
// cluster's services (its API says they're there; nothing was guessed).
func (srv *Server) handleMapFound(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since := now.Add(-mapFoundOldest)
	if ms, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && time.UnixMilli(ms).After(since) {
		since = time.UnixMilli(ms)
	}
	g := srv.mapGraph(now)
	svc := map[string]mapgraph.Service{}
	for _, s := range g.Services {
		svc[s.ID] = s
	}
	firsts := srv.serviceSightings(now)
	var ss []MapFoundService
	for id, first := range firsts {
		s, ok := svc[id]
		if !ok || !first.After(since) || s.Source == "outside" || s.Source == "kubernetes" || s.Declared {
			continue
		}
		var hs []string
		for _, p := range s.Parts {
			hs = appendNew(hs, p.Host)
		}
		ss = append(ss, MapFoundService{ID: id, Name: s.Name, Kind: s.Kind, Hosts: hs[:min(len(hs), 5)], First: first})
	}
	var es []MapFoundEdge
	for _, e := range srv.mapEdges(g, now) {
		if e.Source != "learned" || e.First == nil || !e.First.After(since) {
			continue
		}
		es = append(es, MapFoundEdge{From: e.From, To: e.To, FromName: svc[e.From].Name, ToName: svc[e.To].Name,
			Port: e.Port, Stage: e.Stage, First: *e.First})
	}
	slices.SortFunc(ss, func(a, b MapFoundService) int {
		return cmp.Or(b.First.Compare(a.First), strings.Compare(a.Name, b.Name))
	})
	slices.SortFunc(es, func(a, b MapFoundEdge) int {
		return cmp.Or(b.First.Compare(a.First), strings.Compare(a.FromName, b.FromName), strings.Compare(a.ToName, b.ToName))
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"since": since, "services": ss[:min(len(ss), mapFoundMax)], "edges": es[:min(len(es), mapFoundMax)],
		"service_count": len(ss), "edge_count": len(es),
	})
}

// serviceSightings is when each service was first seen: from map_services,
// reread like the edges, or this copy's memory without a database.
func (srv *Server) serviceSightings(now time.Time) map[string]time.Time {
	srv.maps.mu.Lock()
	defer srv.maps.mu.Unlock()
	if srv.db == nil {
		out := make(map[string]time.Time, len(srv.maps.svcMem))
		for id, r := range srv.maps.svcMem {
			out[id] = r.First
		}
		return out
	}
	if srv.maps.svcFirst != nil && now.Sub(srv.maps.svcAt) < mapEdgesRead {
		return srv.maps.svcFirst
	}
	rows, err := srv.db.MapServices()
	if err != nil {
		log.Printf("maps: can't read when services were first seen: %v", err)
		return srv.maps.svcFirst
	}
	m := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		m[r.ID] = r.First
	}
	srv.maps.svcFirst, srv.maps.svcAt = m, now
	return m
}

// recordServices is the evaluator's, beside the edges: each service with
// parts, at most once an hour.
func (srv *Server) recordServices(g *mapgraph.Graph, now time.Time) {
	hour := now.Unix() / 3600
	srv.maps.mu.Lock()
	if srv.maps.svcRecorded == nil {
		srv.maps.svcRecorded = map[string]int64{}
	}
	var due []string
	for _, s := range g.Services {
		if len(s.Parts) > 0 && srv.maps.svcRecorded[s.ID] != hour {
			due = append(due, s.ID)
		}
	}
	if srv.db == nil {
		if srv.maps.svcMem == nil {
			srv.maps.svcMem = map[string]*state.ServiceRow{}
		}
		for _, id := range due {
			r := srv.maps.svcMem[id]
			if r == nil {
				r = &state.ServiceRow{ID: id, First: now}
				srv.maps.svcMem[id] = r
			}
			r.Last = now
			srv.maps.svcRecorded[id] = hour
		}
		for id, r := range srv.maps.svcMem {
			if now.Sub(r.Last) > edgeDropped {
				delete(srv.maps.svcMem, id)
			}
		}
		srv.maps.mu.Unlock()
		return
	}
	srv.maps.mu.Unlock()
	if len(due) == 0 {
		return
	}
	if err := srv.db.SeenServices(due, now); err != nil {
		log.Printf("maps: can't record %d services: %v", len(due), err)
		return
	}
	srv.maps.mu.Lock()
	for _, id := range due {
		srv.maps.svcRecorded[id] = hour
	}
	srv.maps.svcAt = time.Time{}
	srv.maps.mu.Unlock()
}
