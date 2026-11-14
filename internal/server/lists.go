// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Lists sized for a fleet: the overview carries totals and only the hosts
// most in need, and the full lists are filtered and paged here rather than
// in the browser. At a thousand hosts the whole overview was over a
// megabyte every five seconds.

// overviewHosts is how many hosts the overview sends in full. A fleet this
// size or smaller gets every host, as before.
const overviewHosts = 24

// FleetSummary is what the dashboard needs about every host, without the
// hosts themselves.
type FleetSummary struct {
	Hosts     int `json:"hosts"`
	Reporting int `json:"reporting"`
	Busiest   *struct {
		Host string  `json:"host"`
		CPU  float64 `json:"cpu_pct"`
	} `json:"busiest,omitempty"`
	Fullest *struct {
		Host  string  `json:"host"`
		Mount string  `json:"mount"`
		Pct   float64 `json:"pct"`
	} `json:"fullest,omitempty"`
	// critical when a host with container problems is itself critical.
	ContainerSeverity string `json:"container_severity"`
	// Hosts needing a look, for the sidebar's Hosts badge.
	Warning  int `json:"warning"`
	Critical int `json:"critical"`
}

func summarize(hosts []HostView) FleetSummary {
	f := FleetSummary{Hosts: len(hosts), ContainerSeverity: "warning"}
	for _, h := range hosts {
		if h.Containers.Problems > 0 && h.Status == "critical" {
			f.ContainerSeverity = "critical"
		}
		switch h.Status {
		case "warning":
			f.Warning++
		case "critical":
			f.Critical++
		}
		if h.LastSeen == nil || h.Silent {
			continue
		}
		f.Reporting++
		if f.Busiest == nil || h.CPU > f.Busiest.CPU {
			f.Busiest = &struct {
				Host string  `json:"host"`
				CPU  float64 `json:"cpu_pct"`
			}{h.Name, h.CPU}
		}
		if h.Fullest != nil && (f.Fullest == nil || h.Fullest.Pct > f.Fullest.Pct) {
			f.Fullest = &struct {
				Host  string  `json:"host"`
				Mount string  `json:"mount"`
				Pct   float64 `json:"pct"`
			}{h.Name, h.Fullest.Mount, h.Fullest.Pct}
		}
	}
	return f
}

// need orders hosts most in need first.
func need(status string) int {
	switch status {
	case "critical":
		return 0
	case "warning":
		return 1
	case "waiting":
		return 2
	}
	return 3
}

func byNeed(a, b HostView) int {
	return cmp.Or(cmp.Compare(need(a.Status), need(b.Status)), strings.Compare(a.Name, b.Name))
}

// overviewList is every host for a small fleet, or the ones most in need.
func overviewList(hosts []HostView) []HostView {
	if len(hosts) <= overviewHosts {
		return hosts
	}
	out := slices.Clone(hosts)
	slices.SortFunc(out, byNeed)
	return out[:overviewHosts]
}

// page reads offset and limit, with limit kept between 1 and most.
func page(r *http.Request, def, most int) (offset, limit int) {
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 {
		limit = def
	}
	return max(offset, 0), min(limit, most)
}

func window[T any](all []T, offset, limit int) []T {
	if offset >= len(all) {
		return []T{}
	}
	return all[offset:min(offset+limit, len(all))]
}

// HostRow is a host in the hosts list: what a table row shows.
type HostRow struct {
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Word        string           `json:"word"`
	LastSeen    *time.Time       `json:"last_seen"`
	Silent      bool             `json:"silent"`
	Workstation bool             `json:"workstation,omitempty"`
	OS          string           `json:"os,omitempty"`
	Uptime      float64          `json:"uptime_s"`
	CPU         float64          `json:"cpu_pct"`
	MemPct      float64          `json:"mem_pct"`
	Fullest     *DiskView        `json:"fullest,omitempty"`
	Containers  ContainerSummary `json:"containers"`
}

// handleHosts is the hosts list, most in need first:
// ?q= (name or OS), ?status= (critical, warning, waiting, good, or
// problems for the first two), ?offset=, ?limit= (100, at most 500).
func (srv *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, _ := srv.view(time.Now())
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	status := r.URL.Query().Get("status")
	offset, limit := page(r, 100, 500)
	counts := map[string]int{"all": 0, "critical": 0, "warning": 0, "waiting": 0, "good": 0}
	rows := []HostRow{}
	slices.SortFunc(hosts, byNeed)
	for _, h := range hosts {
		if q != "" && !strings.Contains(strings.ToLower(h.Name+" "+h.OS), q) {
			continue
		}
		counts["all"]++
		counts[h.Status]++
		switch {
		case status == "", status == h.Status:
		case status == "problems" && (h.Status == "critical" || h.Status == "warning"):
		default:
			continue
		}
		rows = append(rows, HostRow{Name: h.Name, Status: h.Status, Word: h.Word, LastSeen: h.LastSeen, Silent: h.Silent,
			Workstation: h.Workstation, OS: h.OS, Uptime: h.Uptime, CPU: h.CPU, MemPct: h.MemPct, Fullest: h.Fullest, Containers: h.Containers})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": window(rows, offset, limit), "total": len(rows),
		"offset": offset, "limit": limit, "counts": counts})
}

// containerShow is the containers page's "show" filter.
func containerShow(show string, c ContainerRow) bool {
	switch show {
	case "running":
		return c.Verdict.Severity == "good"
	case "problems":
		return c.Verdict.Severity == "critical" || c.Verdict.Severity == "warning"
	case "stopped":
		return c.Verdict.Word == "Stopped on purpose"
	}
	return true
}

var containerRank = map[string]int{"critical": 0, "warning": 1, "good": 2, "off": 3}

// handleContainers is every container on every host, problems first:
// ?q= (name, compose project or image), ?host=, ?show= (running,
// problems, stopped), ?offset=, ?limit= (200, at most 1000). counts are
// for q and host, so each "show" choice can say how many it holds.
func (srv *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	hosts, _ := srv.view(time.Now())
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	host, show := r.URL.Query().Get("host"), r.URL.Query().Get("show")
	offset, limit := page(r, 200, 1000)
	var sum ContainerSummary
	names := []string{}
	counts := map[string]int{"all": 0, "running": 0, "problems": 0, "stopped": 0}
	rows := []ContainerRow{}
	for _, h := range hosts {
		sum.add(h.Containers)
		if len(h.rows) > 0 {
			names = append(names, h.Name)
		}
		if host != "" && h.Name != host {
			continue
		}
		for _, c := range h.rows {
			if q != "" && !strings.Contains(strings.ToLower(c.Name+" "+c.Project+" "+c.Image), q) {
				continue
			}
			counts["all"]++
			for _, s := range []string{"running", "problems", "stopped"} {
				if containerShow(s, c) {
					counts[s]++
				}
			}
			if containerShow(show, c) {
				rows = append(rows, c)
			}
		}
	}
	slices.Sort(names)
	slices.SortStableFunc(rows, func(a, b ContainerRow) int {
		ra, ok := containerRank[a.Verdict.Severity]
		if !ok {
			ra = 9
		}
		rb, ok := containerRank[b.Verdict.Severity]
		if !ok {
			rb = 9
		}
		return cmp.Or(cmp.Compare(ra, rb), strings.Compare(a.Host, b.Host), strings.Compare(a.Name, b.Name))
	})
	writeJSON(w, http.StatusOK, map[string]any{"containers": srv.logsFor(r, window(rows, offset, limit)), "total": len(rows),
		"offset": offset, "limit": limit, "counts": counts, "hosts": names, "summary": sum,
		"expected_source": srv.expectedSource(), "expected_hosts": len(srv.currentExpected().Hosts)})
}
