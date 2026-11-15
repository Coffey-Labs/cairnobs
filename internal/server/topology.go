package server

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/units"
)

// Topology is topology.yaml, written by hand: corrections to the maps
// obs-beacond draws from what the agents see (maps.go, docs/maps.md). It
// names the site and says what each host does, and declares services: the
// containers and units each claims, its tier and what it needs (edges the
// maps draw and alerts group by), and the checks that judge it.
type Topology struct {
	Network  NetworkSpec   `yaml:"network"`
	Services []ServiceSpec `yaml:"services"`
}

// NetworkSpec names the site and the hosts' roles.
type NetworkSpec struct {
	Site   string     `yaml:"site"`   // the name of the site holding most of these hosts, e.g. "Datacenter · Rack A"
	Hosts  []RoleHost `yaml:"hosts"`  // hosts in the site
	Remote []RoleHost `yaml:"remote"` // hosts outside it (a desk machine at home, say)
}

// RoleHost is a host and a short line of what it does, shown on its card.
type RoleHost struct {
	Name  string `yaml:"name"`
	Roles string `yaml:"roles"` // e.g. "mail · Gitea · ihasvpn"
}

// tiers are the service map's rows, top to bottom.
var tiers = []string{"edge", "public", "apps", "data"}

// ServiceSpec is a declared service. Its state is the worst of its
// signals; what it needs are edges on the service map.
type ServiceSpec struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Tier       string   `yaml:"tier"`       // edge, public, apps or data (hosts are added below)
	Needs      []string `yaml:"needs"`      // other service ids, or host names
	Checks     []string `yaml:"checks"`     // check ids
	Containers []string `yaml:"containers"` // host/name patterns, "*" for any host
	Units      []string `yaml:"units"`      // host/unit patterns, "*" for any host
	Cloudflare bool     `yaml:"cloudflare"` // takes the Cloudflare conditions as its own
}

// LoadTopology reads and checks a topology file.
func LoadTopology(p string, agents []AgentConfig) (*Topology, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var t Topology
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	hosts := map[string]bool{}
	for _, a := range agents {
		hosts[a.Name] = true
	}
	onSite := map[string]bool{}
	for _, h := range t.Network.Hosts {
		if !hosts[h.Name] {
			return nil, fmt.Errorf("%s: network host %q isn't an agent", p, h.Name)
		}
		onSite[h.Name] = true
	}
	remote := map[string]bool{}
	for _, r := range t.Network.Remote {
		switch {
		case !hosts[r.Name]:
			return nil, fmt.Errorf("%s: remote host %q isn't an agent", p, r.Name)
		case onSite[r.Name] || remote[r.Name]:
			return nil, fmt.Errorf("%s: remote host %q is listed twice", p, r.Name)
		}
		remote[r.Name] = true
	}
	ids := map[string]bool{}
	for i := range t.Services {
		s := &t.Services[i]
		if s.ID == "" || ids[s.ID] || hosts[s.ID] {
			return nil, fmt.Errorf("%s: service %d: missing, repeated or host-named id %q", p, i+1, s.ID)
		}
		ids[s.ID] = true
		if !slices.Contains(tiers, s.Tier) {
			return nil, fmt.Errorf("%s: service %s: tier must be one of %s", p, s.ID, strings.Join(tiers, ", "))
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		for _, pat := range append(append([]string{}, s.Containers...), s.Units...) {
			h, n, ok := strings.Cut(pat, "/")
			if !ok || n == "" || (h != "*" && !hosts[h]) {
				return nil, fmt.Errorf("%s: service %s: %q must be host/name or */name", p, s.ID, pat)
			}
			if _, err := path.Match(n, ""); err != nil {
				return nil, fmt.Errorf("%s: service %s: bad pattern %q", p, s.ID, pat)
			}
		}
	}
	for _, s := range t.Services {
		for _, n := range s.Needs {
			if !ids[n] && !hosts[n] {
				return nil, fmt.Errorf("%s: service %s needs %q, which is neither a service nor a host", p, s.ID, n)
			}
		}
	}
	return &t, nil
}

// topologyFile keeps topology.yaml current, like checks.yaml.
type topologyFile struct {
	mu   sync.Mutex
	path string
	stat os.FileInfo
	cur  *Topology
}

func (srv *Server) currentTopology() *Topology {
	f := &srv.topology
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path == "" {
		return f.cur
	}
	st, err := os.Stat(f.path)
	if err != nil || (f.stat != nil && st.ModTime().Equal(f.stat.ModTime()) && st.Size() == f.stat.Size()) {
		return f.cur
	}
	first := f.stat == nil
	f.stat = st
	t, err := LoadTopology(f.path, srv.agentList())
	if err != nil {
		log.Printf("topology: %v; keeping the last good one", err)
		return f.cur
	}
	f.cur = t
	if !first {
		srv.audit("obs-beacond", "reloaded the topology", f.path)
	}
	return t
}

// Signal is one thing that makes up a service's state.
type Signal struct {
	Kind   string `json:"kind"` // check, container, unit, cloudflare
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"` // good, warning, critical, off, waiting
	Word   string `json:"word"`
	Href   string `json:"href,omitempty"`
}

// ServiceView is a declared service with its live state, judged from its
// signals.
type ServiceView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Tier    string   `json:"tier"`
	Needs   []string `json:"needs"`
	Status  string   `json:"status"`
	Word    string   `json:"word"`
	Signals []Signal `json:"signals"`
}

var statusRank = map[string]int{"good": 0, "off": 1, "waiting": 2, "warning": 3, "serious": 4, "critical": 5}

func worst(a, b string) string {
	if statusRank[b] > statusRank[a] {
		return b
	}
	return a
}

func hostMatch(pat string, host, name string) bool {
	h, n, _ := strings.Cut(pat, "/")
	if h != "*" && h != host {
		return false
	}
	ok, _ := path.Match(n, name)
	return ok
}

// serviceStates judges each declared service from its checks, containers,
// units and Cloudflare (ok false without a topology). Status history and
// reports keep these.
func (srv *Server) serviceStates(now time.Time) (_ []ServiceView, ok bool) {
	t := srv.currentTopology()
	if t == nil {
		return nil, false
	}
	hosts, _ := srv.view(now)
	checks, _ := srv.judgeChecks(now)
	checkBy := map[string]CheckView{}
	for _, c := range checks {
		checkBy[c.ID] = c
	}
	var cfConds []Condition
	if srv.cf != nil {
		cfConds = srv.cloudflareConditions(now)
	}
	services := []ServiceView{}
	for _, s := range t.Services {
		v := ServiceView{ID: s.ID, Name: s.Name, Tier: s.Tier, Needs: append([]string{}, s.Needs...), Signals: []Signal{}}
		for _, id := range s.Checks {
			c, ok := checkBy[id]
			if !ok {
				v.Signals = append(v.Signals, Signal{Kind: "check", Name: id, Status: "waiting", Word: "No such check", Href: "/checks"})
				continue
			}
			v.Signals = append(v.Signals, Signal{Kind: "check", Name: c.Name, Status: c.Status, Word: c.Word, Href: "/checks"})
		}
		for _, h := range hosts {
			for _, row := range h.rows {
				for _, pat := range s.Containers {
					if hostMatch(pat, h.Name, row.Name) {
						v.Signals = append(v.Signals, Signal{Kind: "container", Name: row.Name, Host: h.Name, Status: row.Verdict.Severity, Word: row.Verdict.Word, Href: "/containers"})
						break
					}
				}
			}
			for _, u := range h.Units {
				for _, pat := range s.Units {
					hp, np, _ := strings.Cut(pat, "/")
					if hostMatch(hp+"/"+units.Name(np), h.Name, u.Name) {
						v.Signals = append(v.Signals, Signal{Kind: "unit", Name: u.Name, Host: h.Name, Status: u.Severity, Word: u.Word, Href: "/hosts/" + h.Name})
						break
					}
				}
			}
		}
		if s.Cloudflare && srv.cf != nil {
			st, word := "good", "No problems"
			for _, c := range cfConds {
				st = worst(st, c.Severity)
				word = c.Title
			}
			v.Signals = append(v.Signals, Signal{Kind: "cloudflare", Name: "Cloudflare", Status: st, Word: word, Href: "/cloudflare"})
		}
		// Parts still waiting for their first result don't grey out a
		// service whose other parts have answered.
		v.Status, v.Word = "good", "OK"
		bad, answered := 0, 0
		off := 0
		for _, sg := range v.Signals {
			if sg.Status == "waiting" {
				continue
			}
			answered++
			if sg.Status == "off" { // stopped on purpose: not a problem, and not "up" either
				off++
				continue
			}
			v.Status = worst(v.Status, sg.Status)
			if sg.Status == "warning" || sg.Status == "critical" {
				bad++
			}
		}
		switch {
		case len(v.Signals) == 0:
			v.Status, v.Word = "waiting", "Nothing watched yet"
		case answered == 0:
			v.Status, v.Word = "waiting", "Waiting for results"
		case off == answered:
			v.Status, v.Word = "off", "Stopped on purpose"
		}
		if bad > 0 {
			v.Word = plural(bad, "problem", "problems")
		}
		services = append(services, v)
	}
	return services, true
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
