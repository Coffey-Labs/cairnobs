package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Image drift: the same container running different
// images on different hosts. A deploy rolls host by host, so it's only a
// problem once it has lasted driftAfter.
const driftAfter = 15 * time.Minute

type driftWatch struct {
	mu    sync.Mutex
	since map[string]time.Time // container name → when its hosts started disagreeing
}

// imageDrift looks at running containers by name across the fresh hosts.
func (srv *Server) imageDrift(hosts []HostView, now time.Time) []Condition {
	images := map[string]map[string]string{} // name → host → image
	for _, h := range hosts {
		if h.Silent || h.LastSeen == nil {
			continue
		}
		for _, row := range h.rows {
			if row.State != "running" || row.Image == "" {
				continue
			}
			if images[row.Name] == nil {
				images[row.Name] = map[string]string{}
			}
			images[row.Name][h.Name] = row.Image
		}
	}
	srv.drift.mu.Lock()
	defer srv.drift.mu.Unlock()
	if srv.drift.since == nil {
		srv.drift.since = map[string]time.Time{}
	}
	var conds []Condition
	seen := map[string]bool{}
	for name, by := range images {
		distinct := map[string]bool{}
		for _, img := range by {
			distinct[img] = true
		}
		if len(by) < 2 || len(distinct) < 2 {
			continue
		}
		seen[name] = true
		first, ok := srv.drift.since[name]
		if !ok {
			srv.drift.since[name] = now
			continue
		}
		if now.Sub(first) < driftAfter {
			continue
		}
		var hostsSorted []string
		for h := range by {
			hostsSorted = append(hostsSorted, h)
		}
		sort.Strings(hostsSorted)
		var parts []string
		for _, h := range hostsSorted {
			parts = append(parts, h+" runs "+by[h])
		}
		conds = append(conds, Condition{Key: "drift/" + name, Severity: "warning",
			Title:  fmt.Sprintf("%s runs different images on different hosts", name),
			Detail: strings.Join(parts, "; ") + fmt.Sprintf(". For %s now: a deploy that didn't reach every host?", human(now.Sub(first))), Href: "/containers"})
	}
	for name := range srv.drift.since {
		if !seen[name] {
			delete(srv.drift.since, name)
		}
	}
	sort.Slice(conds, func(i, j int) bool { return conds[i].Key < conds[j].Key })
	return conds
}
