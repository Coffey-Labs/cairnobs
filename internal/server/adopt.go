package server

// What should run where, set in the UI ("adopt what's running now").
// Without `expected` in obs-beacond.yaml, an admin
// opens a host and picks, from what its agent reports, which containers
// must stay running, which are stopped on purpose, and which systemd
// services must stay active. The result is kept in the database as the
// expected.yaml it would be; export and import work on that file. With
// `expected` set, the file wins and the host page shows it read-only.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

const sectionExpected = "expected"

func (srv *Server) expectedSource() string {
	switch {
	case srv.cfg.Expected != "":
		return sourceFile
	case srv.db != nil:
		return sourceUI
	}
	return ""
}

// loadExpectedSetting reads the UI's expectations at start.
func (srv *Server) loadExpectedSetting() error {
	if srv.expectedSource() != sourceUI {
		return nil
	}
	s, ok, err := srv.db.Setting(sectionExpected)
	if err != nil || !ok {
		return err
	}
	e, err := parseExpected([]byte(s.YAML))
	if err != nil {
		return err
	}
	srv.expected = e
	return nil
}

// putExpected stores a new set of expectations and puts it in force; the
// caller holds srv.expMu.
func (srv *Server) putExpected(e *Expected, who string) error {
	if err := srv.saveSection(sectionExpected, e, who); err != nil {
		return err
	}
	srv.expected = e
	return nil
}

// adoptContainer is a container on the host now, as the adopt form lists it.
type adoptContainer struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Status string `json:"status"`
}

// hostExpectations is one host's lists as the UI edits them.
type hostExpectations struct {
	Running []string      `json:"running"`
	Stopped []StoppedRule `json:"stopped"`
	Units   []string      `json:"units"`
}

func (srv *Server) handleSettingsExpected(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	var last *report.Report
	var seen bool
	for _, h := range srv.store.Snapshot() {
		if h.name == host {
			last, seen = &h.last, !h.seen.IsZero()
		}
	}
	if last == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No host called " + host + "."})
		return
	}
	srv.expMu.Lock()
	e := srv.expected
	srv.expMu.Unlock()
	if srv.expectedSource() == sourceFile {
		e = srv.currentExpected()
	}
	eh := e.Hosts[host]
	out := map[string]any{
		"source": srv.expectedSource(), "file": srv.cfg.Expected, "host": host, "reported": seen,
		"expected": hostExpectations{Running: nonNilSlice(eh.Running), Stopped: nonNilSlice(eh.Stopped), Units: nonNilSlice(eh.Units)},
		"ci_jobs":  nonNilSlice(e.CIJobs),
		"windows":  windowsHost(last.OS), // services are Windows', named without .service
	}
	// What's there now. Null, not empty, when the agent can't say: no
	// Docker, no systemd, or an agent from before services were reported.
	if c := last.Containers; c != nil {
		if c.Error != "" {
			out["containers_error"] = c.Error
		}
		list := []adoptContainer{}
		for _, ct := range c.List {
			if _, ci := matches(e.CIJobs, ct.Name); !ci {
				list = append(list, adoptContainer{ct.Name, ct.State, ct.Status})
			}
		}
		slices.SortFunc(list, func(a, b adoptContainer) int { return strings.Compare(a.Name, b.Name) })
		out["containers"] = list
	} else {
		out["containers"] = nil
	}
	if last.Services != nil {
		out["services"] = slices.Sorted(slices.Values(last.Services))
	} else {
		out["services"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExpectedPut replaces one host's lists. Empty lists take the host
// out of the file, so it's judged on Docker's state alone again.
func (srv *Server) handleExpectedPut(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.expectedSource(), "expected containers and services", srv.cfg.Expected) {
		return
	}
	host := r.PathValue("host")
	if !srv.knownHosts()[host] {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No host called " + host + "."})
		return
	}
	var f hostExpectations
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	eh := ExpectedHost{Running: tidyList(f.Running), Units: tidyList(f.Units)}
	seen := map[string]bool{}
	for _, s := range f.Stopped {
		s.Match, s.Note = strings.TrimSpace(s.Match), strings.TrimSpace(s.Note)
		if s.Match != "" && !seen[s.Match] {
			seen[s.Match] = true
			eh.Stopped = append(eh.Stopped, s)
		}
	}
	for _, p := range eh.Running {
		if seen[p] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("%s can't be both expected running and stopped on purpose.", p)})
			return
		}
	}
	if err := eh.check(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	next := &Expected{CIJobs: srv.expected.CIJobs, Hosts: map[string]ExpectedHost{}, Clusters: srv.expected.Clusters}
	for h, x := range srv.expected.Hosts {
		next.Hosts[h] = x
	}
	if len(eh.Running)+len(eh.Stopped)+len(eh.Units) == 0 {
		delete(next.Hosts, host)
	} else {
		next.Hosts[host] = eh
	}
	if len(next.Hosts) == 0 {
		next.Hosts = nil
	}
	if err := srv.putExpected(next, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(srv.who(r), "set what should run on "+host, fmt.Sprintf("%s running, %s stopped on purpose, %s",
		plural(len(eh.Running), "container", "containers"), plural(len(eh.Stopped), "container", "containers"), plural(len(eh.Units), "service", "services")))
	writeJSON(w, http.StatusOK, hostExpectations{Running: nonNilSlice(eh.Running), Stopped: nonNilSlice(eh.Stopped), Units: nonNilSlice(eh.Units)})
}

// tidyList trims, drops blanks and duplicates, and keeps the order.
func tidyList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// exportExpected is what should run where, as expected.yaml.
func (srv *Server) exportExpected() any {
	if srv.expectedSource() == sourceFile {
		return srv.currentExpected()
	}
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	return srv.expected
}

// importExpected replaces the UI's expectations with an expected.yaml.
func (srv *Server) importExpected(b []byte, who string) (int, error) {
	e, err := parseExpected(b)
	if err != nil {
		return 0, err
	}
	known := srv.knownHosts()
	for h := range e.Hosts {
		if !known[h] {
			return 0, fmt.Errorf("no host called %q", h)
		}
	}
	for c := range e.Clusters {
		if !slices.Contains(srv.clusterAgents(), c) {
			return 0, fmt.Errorf("no cluster called %q", c)
		}
	}
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	if err := srv.putExpected(e, who); err != nil {
		return 0, err
	}
	return len(e.Hosts), nil
}
