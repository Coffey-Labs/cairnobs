package server

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// The Hosts page's grid (#72): one square per host, grouped by site (the
// network map's) and role, so at thousands of hosts the odd one out still
// stands out. GET /api/v1/fleet; not under /hosts/, where a host named
// "grid" would lose its page.

// GridSite is a site's hosts by role.
type GridSite struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Roles []GridRole `json:"roles"`
	Hosts int        `json:"hosts"`
	// Trouble is its hosts that need a look, worst first.
	Trouble []GridHost `json:"trouble"`
}

// GridRole is one role's hosts in a site.
type GridRole struct {
	Role  string     `json:"role"`
	Hosts []GridHost `json:"hosts"`
}

// GridHost is one square.
type GridHost struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Word   string `json:"word"`
}

// hostRole is what a host is from its name: the first word after its
// site, without its number. phx1-app05 in phx1 is "app", web-02 is "web",
// win-dc01 is "win", host3 is "host".
func hostRole(name, site string) string {
	r := name
	if site != "" && strings.HasPrefix(r, site) {
		r = r[len(site):]
	}
	r = strings.Trim(r, "-_.")
	if i := strings.IndexAny(r, "-_."); i >= 0 {
		r = r[:i]
	}
	r = strings.TrimRight(r, "0123456789")
	if r == "" {
		return "other"
	}
	return r
}

// fleetGrid groups hosts by site and role. siteOf names each host's site
// (id → name in names); hosts in none go under "Elsewhere".
func fleetGrid(hosts []HostView, siteOf map[string]string, names map[string]string) []GridSite {
	by := map[string]*GridSite{}
	roles := map[string]map[string]*GridRole{}
	// A host the map didn't place (a collector, one without map samples)
	// goes with the site its name starts with, when one does: phx1-stor01
	// with phx1.
	prefixes := map[string]string{}
	for id, n := range names {
		if n != "" {
			prefixes[n] = id
		}
	}
	for _, h := range hosts {
		id := siteOf[h.Name]
		if id == "" {
			if p, _, ok := strings.Cut(h.Name, "-"); ok && prefixes[p] != "" {
				id = prefixes[p]
			}
		}
		s := by[id]
		if s == nil {
			name := names[id]
			if id == "" {
				name = "Elsewhere"
			}
			s = &GridSite{ID: id, Name: name, Trouble: []GridHost{}}
			by[id] = s
			roles[id] = map[string]*GridRole{}
		}
		prefix := names[id]
		role := hostRole(h.Name, prefix)
		r := roles[id][role]
		if r == nil {
			r = &GridRole{Role: role}
			roles[id][role] = r
		}
		st := h.Status
		if h.Silent {
			st = "critical"
		}
		g := GridHost{Name: h.Name, Status: st, Word: h.Word}
		r.Hosts = append(r.Hosts, g)
		s.Hosts++
		if st == "warning" || st == "serious" || st == "critical" {
			s.Trouble = append(s.Trouble, g)
		}
	}
	out := []GridSite{}
	for id, s := range by {
		for _, r := range roles[id] {
			sort.Slice(r.Hosts, func(i, j int) bool { return r.Hosts[i].Name < r.Hosts[j].Name })
			s.Roles = append(s.Roles, *r)
		}
		// The biggest role first (the app servers), then by name.
		sort.Slice(s.Roles, func(i, j int) bool {
			if len(s.Roles[i].Hosts) != len(s.Roles[j].Hosts) {
				return len(s.Roles[i].Hosts) > len(s.Roles[j].Hosts)
			}
			return s.Roles[i].Role < s.Roles[j].Role
		})
		sort.SliceStable(s.Trouble, func(i, j int) bool { return statusRank[s.Trouble[i].Status] > statusRank[s.Trouble[j].Status] })
		out = append(out, *s)
	}
	// Sites by size, Elsewhere last.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == "") != (out[j].ID == "") {
			return out[j].ID == ""
		}
		if out[i].Hosts != out[j].Hosts {
			return out[i].Hosts > out[j].Hosts
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (srv *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	hosts, _ := srv.view(now)
	siteOf, names := map[string]string{}, map[string]string{}
	if g := srv.mapGraph(now); g != nil {
		for _, s := range g.Sites {
			names[s.ID] = s.Name
			for _, h := range s.Hosts {
				siteOf[h] = s.ID
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": fleetGrid(hosts, siteOf, names), "watch": srv.watch(hosts, now)})
}
