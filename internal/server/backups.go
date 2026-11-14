package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/backup"
	"github.com/cairnobs/cairnobs/internal/report"
)

// scanEvery is how often obs-beacon-scan.timer runs (hosts-baseline); a scan
// four times older than this means the timer has stopped.
const scanEvery = 15 * time.Minute

func loadBackups(p string, agents []AgentConfig) (*backup.File, error) {
	f, err := backup.Load(p)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, a := range agents {
		known[a.Name] = true
	}
	for h, spec := range f.Hosts {
		if !known[h] {
			return nil, fmt.Errorf("%s: no agent called %q", p, h)
		}
		for _, s := range spec.Received {
			if !known[s.From] {
				return nil, fmt.Errorf("%s: %s receives from %q, which isn't an agent", p, h, s.From)
			}
		}
	}
	return f, nil
}

// backupsFile keeps backups.yaml current the way checksFile does.
type backupsFile struct {
	mu   sync.Mutex
	path string
	stat os.FileInfo
	cur  *backup.File
}

func (srv *Server) currentBackups() *backup.File {
	f := &srv.backups
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path == "" {
		return f.cur
	}
	st, err := os.Stat(f.path)
	if err != nil || (f.stat != nil && st.ModTime().Equal(f.stat.ModTime()) && st.Size() == f.stat.Size()) {
		return f.cur
	}
	f.stat = st
	c, err := loadBackups(f.path, srv.agentList())
	if err != nil {
		log.Printf("backups: %v; keeping the last good file", err)
		return f.cur
	}
	f.cur = c
	srv.audit("obs-beacond", "reloaded the backup expectations", f.path)
	return c
}

// backupScans is each reporting host's latest scan. A silent host is left
// out: its silence alerts on its own, and its last scan stays on the page.
func (srv *Server) backupScans(now time.Time) map[string]*report.Backups {
	out := map[string]*report.Backups{}
	for _, h := range srv.store.Snapshot() {
		if h.seen.IsZero() || now.Sub(h.seen) > srv.cfg.StaleAfter {
			continue
		}
		out[h.name] = h.last.Backups
	}
	return out
}

func (srv *Server) backupConditions(now time.Time) []Condition {
	f := srv.currentBackups()
	if f == nil {
		return nil
	}
	var out []Condition
	scans := srv.backupScans(now)
	hosts := make([]string, 0, len(f.Hosts))
	for h := range f.Hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		b, reporting := scans[h]
		if !reporting {
			continue
		}
		ws := srv.workstation(h)
		for _, x := range f.Judge(h, b, now, scanEvery) {
			sev := x.Severity
			if ws {
				sev = "warning" // a workstation never pages, a failing disk included
			}
			out = append(out, Condition{Key: "backup/" + h + "/" + x.Key, Severity: sev, Title: x.Title, Detail: x.Detail, Host: h, Href: "/backups"})
		}
	}
	return out
}

// BackupHostView is one host on the Backups page: what it expects, its
// latest scan (kept from before it went silent), and what's wrong.
type BackupHostView struct {
	Host     string          `json:"host"`
	Silent   bool            `json:"silent"`
	Expect   backup.Host     `json:"expect"`
	Scan     *report.Backups `json:"scan"`
	Findings []Condition     `json:"findings"`
}

func (srv *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	f := srv.currentBackups()
	if f == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	now := time.Now()
	conds := srv.backupConditions(now)
	views := []BackupHostView{}
	last := map[string]*report.Backups{}
	silent := map[string]bool{}
	for _, h := range srv.store.Snapshot() {
		last[h.name] = h.last.Backups
		silent[h.name] = h.seen.IsZero() || now.Sub(h.seen) > srv.cfg.StaleAfter
	}
	for name, spec := range f.Hosts {
		quiet, known := silent[name]
		v := BackupHostView{Host: name, Silent: quiet || !known, Expect: spec, Scan: last[name], Findings: []Condition{}}
		for _, c := range conds {
			if c.Host == name {
				v.Findings = append(v.Findings, c)
			}
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Host < views[j].Host })
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "hosts": views,
		"max_age_h": f.MaxAge.Hours(), "shrink": f.Shrink, "disk_warn": f.DiskWarn, "scan_every_s": int(scanEvery / time.Second)})
}
