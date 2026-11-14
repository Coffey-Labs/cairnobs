package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/check"
	"github.com/cairnobs/cairnobs/internal/fetch"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/units"
)

// Checks is checks.yaml: the active checks agents run.
type Checks struct {
	Checks []check.Spec `yaml:"checks"`
}

// LoadChecks reads and checks a checks file.
func LoadChecks(p string, agents []AgentConfig) (*Checks, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := parseChecks(b, agentNames(agents))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}

// parseChecks reads a checks document and checks it. Every host in a
// check's `from` must be in known; nil known skips that.
func parseChecks(b []byte, known map[string]bool) (*Checks, error) {
	var c Checks
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := c.check(known); err != nil {
		return nil, err
	}
	return &c, nil
}

// check fills in each check's defaults and refuses a list that can't run.
func (c *Checks) check(known map[string]bool) error {
	ids := map[string]bool{}
	for i := range c.Checks {
		s := &c.Checks[i]
		if err := s.Check(); err != nil {
			return err
		}
		if ids[s.ID] {
			return fmt.Errorf("two checks called %q", s.ID)
		}
		ids[s.ID] = true
		if known == nil {
			continue
		}
		for _, h := range s.From {
			if !known[h] {
				return fmt.Errorf("check %s: no host called %q", s.ID, h)
			}
		}
	}
	return nil
}

func agentNames(agents []AgentConfig) map[string]bool {
	known := map[string]bool{}
	for _, a := range agents {
		known[a.Name] = true
	}
	return known
}

// checksFile keeps checks.yaml current: re-read when it changes on disk; a
// file that doesn't parse is logged and the last good list stays. Without
// a file, the checks are edited in the UI and kept in the database (raw is
// what was entered, before defaults; settings.go).
type checksFile struct {
	mu   sync.Mutex
	path string
	stat os.FileInfo
	cur  *Checks
	raw  []check.Spec
}

func (srv *Server) currentChecks() *Checks {
	f := &srv.checks
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
	c, err := LoadChecks(f.path, srv.agentList())
	if err != nil {
		log.Printf("checks: %v; keeping the last good list", err)
		return f.cur
	}
	f.cur = c
	if !first {
		srv.audit("obs-beacond", "reloaded the checks", fmt.Sprintf("%s (%d checks)", f.path, len(c.Checks)))
	}
	return c
}

// agentWork is what an agent is asked to do beyond its own report: the
// checks it runs and the systemd units it watches.
type agentWork struct {
	Checks  []check.Spec `json:"checks"`
	Units   []string     `json:"units"`
	Fetches []fetch.Spec `json:"fetches"`
}

// workFor is an agent's work and its version (empty: nothing to do).
func (srv *Server) workFor(agent string) (agentWork, string) {
	w := agentWork{Checks: []check.Spec{}, Units: []string{}, Fetches: []fetch.Spec{}}
	// A check or probe with no `from` goes to every server; a workstation
	// gets only those that name it.
	ws := srv.workstation(agent)
	if dl := srv.cfg.DataLayer; dl != nil {
		for _, p := range dl.Probes {
			if (len(p.From) == 0 && !ws) || slices.Contains(p.From, agent) {
				w.Fetches = append(w.Fetches, p.fetchSpec())
			}
		}
	}
	if c := srv.currentChecks(); c != nil {
		for _, s := range c.Checks {
			if (len(s.From) == 0 && !ws) || slices.Contains(s.From, agent) {
				w.Checks = append(w.Checks, s)
			}
		}
	}
	for _, u := range srv.currentExpected().Hosts[agent].Units {
		w.Units = append(w.Units, units.Name(u))
	}
	if len(w.Checks) == 0 && len(w.Units) == 0 && len(w.Fetches) == 0 {
		return w, ""
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return w, hex.EncodeToString(sum[:8])
}

func (srv *Server) handleAgentChecks(w http.ResponseWriter, r *http.Request) {
	agent, ok := srv.agentFor(r)
	if !ok {
		http.Error(w, srv.agentRefusal(), http.StatusUnauthorized)
		return
	}
	work, ver := srv.workFor(agent)
	writeJSON(w, http.StatusOK, map[string]any{"version": ver, "checks": work.Checks, "units": work.Units, "fetches": work.Fetches})
}

// CheckView is one check as the UI shows it, with every host's latest result.
type CheckView struct {
	ID       string                        `json:"id"`
	Name     string                        `json:"name"`
	Kind     string                        `json:"kind"`
	Target   string                        `json:"target"`
	Status   string                        `json:"status"` // good, warning, critical, waiting
	Word     string                        `json:"word"`
	Detail   string                        `json:"detail,omitempty"`
	CertDays *int                          `json:"cert_days,omitempty"`
	Results  map[string]report.CheckResult `json:"results"` // by host; fresh results only
	From     []string                      `json:"from"`
}

// judgeChecks applies the rule that a check is down only when two hosts
// agree (or the only host running it says so). One host failing alone is
// shown, not alerted: it's usually that host's network path.
func (srv *Server) judgeChecks(now time.Time) ([]CheckView, []Condition) {
	views := []CheckView{}
	var conds []Condition
	c := srv.currentChecks()
	if c == nil {
		return views, nil
	}
	latest := map[string]map[string]report.CheckResult{} // check → host → result
	var hosts []string
	for _, h := range srv.store.Snapshot() {
		if !srv.workstation(h.name) { // a check with no `from` runs on the servers only
			hosts = append(hosts, h.name)
		}
		if h.seen.IsZero() || now.Sub(h.seen) > srv.cfg.StaleAfter {
			continue // a silent host's results are old; its silence alerts on its own
		}
		for _, res := range h.last.Checks {
			if latest[res.ID] == nil {
				latest[res.ID] = map[string]report.CheckResult{}
			}
			latest[res.ID][h.name] = res
		}
	}
	const href = "/checks"
	for _, s := range c.Checks {
		v := CheckView{ID: s.ID, Name: s.Name, Kind: s.Kind(), Target: s.Target(), Results: map[string]report.CheckResult{}, From: s.From}
		if len(v.From) == 0 {
			v.From = hosts
		}
		var failing, passing []string
		var soonest time.Time
		for host, res := range latest[s.ID] {
			if now.Sub(res.At) > 2*s.Every+srv.cfg.StaleAfter {
				continue
			}
			v.Results[host] = res
			if res.OK {
				passing = append(passing, host)
			} else {
				failing = append(failing, host)
			}
			if !res.CertExpires.IsZero() && (soonest.IsZero() || res.CertExpires.Before(soonest)) {
				soonest = res.CertExpires
			}
		}
		sort.Strings(failing)
		firstErr := ""
		if len(failing) > 0 {
			firstErr = v.Results[failing[0]].Error
		}
		n := len(failing) + len(passing)
		switch {
		case n == 0:
			v.Status, v.Word = "waiting", "Waiting for results"
		case len(failing) >= 2 || (n == 1 && len(failing) == 1):
			v.Status, v.Word = s.Severity, "Down"
			v.Detail = fmt.Sprintf("Failing from %s: %s", strings.Join(failing, ", "), firstErr)
			conds = append(conds, Condition{Key: "check/" + s.ID, Severity: s.Severity, Title: s.Name + " is down",
				Detail: v.Detail + ".", Href: href})
		case len(failing) == 1:
			v.Status, v.Word = "warning", "Failing from "+failing[0]+" only"
			v.Detail = firstErr + ". Not alerting: the other hosts reach it."
		default:
			v.Status, v.Word = "good", "Up"
		}
		if !soonest.IsZero() {
			days := int(soonest.Sub(now).Hours() / 24)
			v.CertDays = &days
			sev := ""
			switch {
			case days < s.TLSCriticalDays:
				sev = "critical"
			case days < s.TLSWarnDays:
				sev = "warning"
			}
			if sev != "" {
				conds = append(conds, Condition{Key: "cert/" + s.ID, Severity: sev,
					Title:  fmt.Sprintf("%s certificate expires in %s", s.Name, plural(days, "day", "days")),
					Detail: fmt.Sprintf("Seen on %s; it expires %s.", s.Target(), soonest.UTC().Format("2006-01-02 15:04 UTC")), Href: href})
				if v.Status == "good" {
					v.Status, v.Word = sev, "Certificate expiring"
				}
			}
		}
		views = append(views, v)
	}
	return views, conds
}

func (srv *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	views, _ := srv.judgeChecks(time.Now())
	src := srv.checksSource()
	writeJSON(w, http.StatusOK, map[string]any{"checks": views, "configured": src != "", "source": src, "file": srv.cfg.Checks})
}
