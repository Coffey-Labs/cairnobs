package server

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/units"
)

// Topology is topology.yaml, written by hand. The maps now draw themselves
// from discovery (maps.go, docs/maps.md); this file names and corrects what's
// found, and the hand-drawn maps that read it stay until the new pages
// replace them.
type Topology struct {
	Network  NetworkSpec   `yaml:"network"`
	Services []ServiceSpec `yaml:"services"`
}

// NetworkSpec is the network map.
type NetworkSpec struct {
	Site   string       `yaml:"site"`   // the box the hosts sit in, e.g. "Datacenter · Rack A"
	Hosts  []MeshHost   `yaml:"hosts"`  // in drawing order
	Mail   []MailName   `yaml:"mail"`   // mail names that bypass Cloudflare
	Remote []RemoteHost `yaml:"remote"` // agents outside the site, each on its own link to one site host
}

// RemoteHost is an agent outside the site that reaches it over a link of its
// own to one site host (a desk machine at home, say, on its own WireGuard
// tunnel). It's drawn outside the site box with that one line: it isn't a
// mesh member and isn't on the service map.
type RemoteHost struct {
	Name      string `yaml:"name"`
	Via       string `yaml:"via"`        // the site host at the other end of its link
	Link      string `yaml:"link"`       // the link's name, e.g. "wg-bench"
	LinkCheck string `yaml:"link_check"` // a ping check this host runs to the other end; gives the round trip
	MeshIP    string `yaml:"mesh_ip"`    // its address on the link, shown on the card
	Roles     string `yaml:"roles"`      // a short line of what it is, e.g. "workstation · home"
}

// MeshHost is a host and the check that pings it over the private mesh.
type MeshHost struct {
	Name      string `yaml:"name"`
	MeshCheck string `yaml:"mesh_check"` // a ping check other hosts run against this one
	MeshIP    string `yaml:"mesh_ip"`    // its address on the private mesh, shown on the card
	Roles     string `yaml:"roles"`      // a short line of what it does, e.g. "mail · Gitea · ihasvpn"
}

// MailName is a mail server name, the host it lives on and the check that
// tries it.
type MailName struct {
	Name  string `yaml:"name"`
	Host  string `yaml:"host"`
	Check string `yaml:"check"`
}

// tiers are the service map's rows, top to bottom.
var tiers = []string{"edge", "public", "apps", "data"}

// ServiceSpec is one node of the service map. Its state is the worst of
// its signals; its links are what it needs.
type ServiceSpec struct {
	ID         string   `yaml:"id"`
	Name       string   `yaml:"name"`
	Tier       string   `yaml:"tier"`       // edge, public, apps or data (hosts are added below)
	Needs      []string `yaml:"needs"`      // other service ids, or host names
	Checks     []string `yaml:"checks"`     // check ids
	Containers []string `yaml:"containers"` // host/name patterns, "*" for any host
	Units      []string `yaml:"units"`      // host/unit patterns, "*" for any host
	Cloudflare bool     `yaml:"cloudflare"` // takes the Cloudflare conditions as its own
	Sub        string   `yaml:"sub"`        // a few words under its name on the map, e.g. "Patroni · 3 nodes"
	Note       string   `yaml:"note"`
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
		case !onSite[r.Via]:
			return nil, fmt.Errorf("%s: remote host %s: via %q isn't one of the network hosts", p, r.Name, r.Via)
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
	Kind   string `json:"kind"` // check, container, unit, cloudflare, host
	Name   string `json:"name"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"` // good, warning, critical, off, waiting
	Word   string `json:"word"`
	Href   string `json:"href,omitempty"`
}

// ServiceView is a service map node with its live state.
type ServiceView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Tier    string   `json:"tier"`
	Needs   []string `json:"needs"`
	Status  string   `json:"status"`
	Word    string   `json:"word"`
	Sub     string   `json:"sub"`
	Cause   string   `json:"cause,omitempty"` // the deeper failure that explains this one, if any
	Note    string   `json:"note,omitempty"`
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

// LinkView is one mesh link with the round trip measured both ways.
type LinkView struct {
	A, B   string   `json:"-"`
	Hosts  []string `json:"hosts"`
	Ms     *float64 `json:"ms"` // average of both directions; nil when neither has a result
	Status string   `json:"status"`
	Word   string   `json:"word"`
}

// PathView is one public or mail path to a host.
type PathView struct {
	Kind   string `json:"kind"` // public (via Cloudflare) or mail (MX, direct)
	Name   string `json:"name"`
	Host   string `json:"host"`
	Status string `json:"status"`
	Word   string `json:"word"`
}

// topologyView builds both maps' live state.
func (srv *Server) topologyView(now time.Time) map[string]any {
	t := srv.currentTopology()
	if t == nil {
		return map[string]any{"configured": false}
	}
	hosts, _ := srv.view(now)
	checks, _ := srv.judgeChecks(now)
	checkBy := map[string]CheckView{}
	for _, c := range checks {
		checkBy[c.ID] = c
	}
	hostBy := map[string]HostView{}
	var hostNames []string
	for _, h := range hosts {
		hostBy[h.Name] = h
		hostNames = append(hostNames, h.Name)
	}

	// --- services ---
	var cfConds []Condition
	if srv.cf != nil {
		cfConds = srv.cloudflareConditions(now)
	}
	services := []ServiceView{}
	for _, s := range t.Services {
		v := ServiceView{ID: s.ID, Name: s.Name, Tier: s.Tier, Needs: append([]string{}, s.Needs...), Sub: s.Sub, Note: s.Note, Signals: []Signal{}}
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
		if v.Sub == "" { // what it's made of, when the topology doesn't say
			n := map[string]int{}
			for _, sg := range v.Signals {
				n[sg.Kind]++
			}
			var parts []string
			for _, k := range []string{"check", "container", "unit"} {
				if n[k] > 0 {
					parts = append(parts, plural(n[k], k, k+"s"))
				}
			}
			v.Sub = strings.Join(parts, " · ")
		}
		if bad > 0 {
			v.Word = plural(bad, "problem", "problems")
		}
		services = append(services, v)
	}
	// Hosts are the bottom row of the service map.
	for _, h := range t.Network.Hosts {
		hv := hostBy[h.Name]
		services = append(services, ServiceView{ID: h.Name, Name: h.Name, Tier: "hosts", Needs: []string{}, Status: hv.Status, Word: hv.Word, Sub: h.MeshIP,
			Signals: []Signal{{Kind: "host", Name: h.Name, Host: h.Name, Status: hv.Status, Word: hv.Word, Href: "/hosts/" + h.Name}}})
	}

	// A failing service whose trouble is explained by a deeper failure
	// says which (the same reasoning that groups its alerts).
	closure := needsClosure(t)
	failingNode := map[string]bool{}
	nameOf := map[string]string{}
	for _, sv := range services {
		nameOf[sv.ID] = sv.Name
		if sv.Tier == "hosts" {
			// A host is red whenever anything on it is; it's only the cause
			// when the host itself is gone.
			failingNode[sv.ID] = hostBy[sv.ID].Silent
			continue
		}
		if statusRank[sv.Status] >= statusRank["warning"] {
			failingNode[sv.ID] = true
		}
	}
	rootNode := func(n string) bool {
		for m := range closure[n] {
			if failingNode[m] {
				return false
			}
		}
		return true
	}
	for i := range services {
		sv := &services[i]
		if !failingNode[sv.ID] {
			continue
		}
		var causes []string
		for m := range closure[sv.ID] {
			if failingNode[m] && rootNode(m) {
				causes = append(causes, nameOf[m])
			}
		}
		sort.Strings(causes)
		sv.Cause = strings.Join(causes, ", ")
	}

	// --- network ---
	meshCheck := map[string]string{}
	for _, h := range t.Network.Hosts {
		meshCheck[h.Name] = h.MeshCheck
	}
	var links []LinkView
	for i, a := range t.Network.Hosts {
		for _, b := range t.Network.Hosts[i+1:] {
			l := LinkView{A: a.Name, B: b.Name, Hosts: []string{a.Name, b.Name}, Status: "waiting", Word: "No results"}
			var sum float64
			n, failed := 0, 0
			for _, dir := range [][2]string{{a.Name, b.Name}, {b.Name, a.Name}} { // from, to
				c, ok := checkBy[meshCheck[dir[1]]]
				if !ok {
					continue
				}
				if r, ok := c.Results[dir[0]]; ok {
					if r.OK {
						sum += r.Ms
						n++
					} else {
						failed++
					}
				}
			}
			switch {
			case failed == 2:
				l.Status, l.Word = "critical", "Down both ways"
			case failed == 1:
				l.Status, l.Word = "warning", "Failing one way"
			case n > 0:
				l.Status, l.Word = "good", "Up"
			}
			if n > 0 {
				ms := sum / float64(n)
				l.Ms = &ms
			}
			links = append(links, l)
		}
	}
	var paths []PathView
	if srv.cf != nil {
		snap := srv.cf.Snapshot()
		perHost := map[string]string{}
		words := map[string]string{}
		for _, p := range snap.Pools {
			if !p.Enabled {
				continue
			}
			for _, o := range p.Origins {
				if !o.Enabled {
					continue
				}
				st := "good"
				if !o.Healthy {
					st = "warning"
					words[o.Name] = "Failing " + p.Name
				}
				cur, seen := perHost[o.Name]
				if !seen {
					cur = "good"
				}
				perHost[o.Name] = worst(cur, st)
			}
		}
		for _, h := range t.Network.Hosts {
			if st, ok := perHost[h.Name]; ok {
				w := words[h.Name]
				if w == "" {
					w = "Healthy in every pool"
				}
				paths = append(paths, PathView{Kind: "public", Name: "Cloudflare", Host: h.Name, Status: st, Word: w})
			}
		}
	}
	for _, m := range t.Network.Mail {
		p := PathView{Kind: "mail", Name: m.Name, Host: m.Host, Status: "waiting", Word: "No results"}
		if c, ok := checkBy[m.Check]; ok {
			p.Status, p.Word = c.Status, c.Word
		}
		paths = append(paths, p)
	}
	// What runs on each host: every service with a part there, judged by
	// its parts on that host.
	onHost := map[string][]map[string]string{}
	for _, sv := range services {
		if sv.Tier == "hosts" {
			continue
		}
		per := map[string]string{}
		for _, sg := range sv.Signals {
			if sg.Host != "" && sg.Status != "waiting" {
				cur, ok := per[sg.Host]
				if !ok {
					cur = "good"
				}
				per[sg.Host] = worst(cur, sg.Status)
			}
		}
		for h, st := range per {
			onHost[h] = append(onHost[h], map[string]string{"id": sv.ID, "name": sv.Name, "status": st})
		}
	}
	card := func(name, meshIP, roles string) map[string]any {
		hv := hostBy[name]
		disk := 0.0
		if hv.Fullest != nil {
			disk = hv.Fullest.Pct
		}
		svc := onHost[name]
		if svc == nil {
			svc = []map[string]string{}
		}
		sort.Slice(svc, func(i, j int) bool { return statusRank[svc[i]["status"]] > statusRank[svc[j]["status"]] })
		return map[string]any{"name": name, "status": hv.Status, "word": hv.Word, "cpu_pct": hv.CPU, "mem_pct": hv.MemPct,
			"disk_pct": disk, "rx_bps": hv.Net.RxBps, "tx_bps": hv.Net.TxBps, "mesh_ip": meshIP, "roles": roles,
			"os": hv.OS, "kernel": hv.Kernel, "uptime_s": hv.Uptime, "last_seen": hv.LastSeen, "services": svc,
			"workstation": hv.Workstation}
	}
	netHosts := []map[string]any{}
	for _, h := range t.Network.Hosts {
		netHosts = append(netHosts, card(h.Name, h.MeshIP, h.Roles))
	}
	// A remote host's link is up while its reports arrive (they come over
	// it); its own ping to the far end gives the round trip.
	remotes := []map[string]any{}
	for _, r := range t.Network.Remote {
		c := card(r.Name, r.MeshIP, r.Roles)
		hv := hostBy[r.Name]
		lk := map[string]any{"name": r.Link, "via": r.Via, "status": "waiting", "word": "No report yet", "ms": nil}
		switch {
		case hv.Silent:
			lk["status"], lk["word"] = hv.Status, "No reports coming over it"
		case hv.LastSeen != nil:
			lk["status"], lk["word"] = "good", "Up"
			if res, ok := checkBy[r.LinkCheck].Results[r.Name]; ok && r.LinkCheck != "" {
				if res.OK {
					ms := res.Ms
					lk["ms"] = &ms
				} else {
					lk["status"], lk["word"] = "warning", "Reports arrive, but the ping fails"
				}
			}
		}
		c["link"] = lk
		remotes = append(remotes, c)
	}
	// The Cloudflare box and the alert channels box.
	cfView := map[string]any{"configured": srv.cf != nil}
	if srv.cf != nil {
		snap := srv.cf.Snapshot()
		st, word := "good", "OK"
		if snap.Checked.IsZero() {
			st, word = "waiting", "First reading under way"
		}
		for _, c := range cfConds {
			st = worst(st, c.Severity)
			word = c.Title
		}
		pools := []map[string]any{}
		for _, p := range snap.Pools {
			up, total := 0, 0
			for _, o := range p.Origins {
				if o.Enabled {
					total++
					if o.Healthy {
						up++
					}
				}
			}
			pools = append(pools, map[string]any{"name": p.Name, "enabled": p.Enabled, "healthy": p.Healthy, "up": up, "total": total})
		}
		cfView = map[string]any{"configured": true, "status": st, "word": word, "pools": pools, "zones": len(snap.Zones), "cloudflare_status": snap.Status}
	}
	chans := []map[string]string{}
	for _, ch := range srv.alerts.chans() {
		chans = append(chans, map[string]string{"id": ch.ID(), "type": ch.Type(), "min": ch.Min()})
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].A+links[i].B < links[j].A+links[j].B })
	return map[string]any{
		"configured": true,
		"tiers":      append(append([]string{}, tiers...), "hosts"),
		"services":   services,
		"network": map[string]any{"site": t.Network.Site, "hosts": netHosts, "remote": remotes, "links": nonNilSlice(links), "paths": nonNilSlice(paths),
			"cloudflare": cfView, "channels": chans},
	}
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (srv *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, srv.topologyView(time.Now()))
}
