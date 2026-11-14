package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// What clusters' nodes, controllers and VMs use (docs/cluster-watcher.md,
// step 5). A cluster agent sends a reading a minute with its batches; it's
// kept in workload_usage, one row per cluster, so every copy shows the
// same, and summed into hourly history by the recorder (history.go).

// usageFresh is how old a reading may be and still be shown: a cluster
// agent reads every minute.
const usageFresh = 3 * time.Minute

func usageByID(us []report.Usage) map[string]report.Usage {
	m := make(map[string]report.Usage, len(us))
	for _, u := range us {
		m[u.ID] = u
	}
	return m
}

// syncUsage reads each cluster's reading when it's newer than this copy's.
// It runs with syncMu held, after the records.
func (srv *Server) syncUsage() {
	ws := &srv.wl
	times, err := srv.db.WorkloadUsageTimes()
	if err != nil {
		log.Printf("workloads: usage: %v", err)
		return
	}
	for src, at := range times {
		ws.mu.RLock()
		s := ws.src[src]
		have := s == nil || !s.usageAt.Before(at)
		ws.mu.RUnlock()
		if have {
			continue
		}
		got, j, err := srv.db.WorkloadUsage(src)
		var us []report.Usage
		if err == nil {
			err = json.Unmarshal(j, &us)
		}
		if err != nil {
			log.Printf("workloads: %s's usage: %v", src, err)
			continue
		}
		m := usageByID(us)
		ws.mu.Lock()
		if s := ws.src[src]; s != nil && got.After(s.usageAt) {
			s.usageAt, s.usage = got, m
		}
		ws.mu.Unlock()
	}
}

// UsageNow is a record's usage on the pages, with how full it is where
// there's a ceiling.
type UsageNow struct {
	CPU    float64    `json:"cpu"` // cores
	Mem    int64      `json:"mem"` // bytes
	CPUOf  float64    `json:"cpu_of,omitempty"`
	MemOf  int64      `json:"mem_of,omitempty"`
	CPUPct float64    `json:"cpu_pct,omitempty"` // of CPUOf
	MemPct float64    `json:"mem_pct,omitempty"` // of MemOf
	IO     *report.IO `json:"io,omitempty"`      // an array's
	Leases *int64     `json:"leases,omitempty"`  // a Vault cluster's
	At     time.Time  `json:"at"`
}

// usageOf is rec's usage now, or nil when there's no fresh reading for it.
func (s wsnap) usageOf(id string, now time.Time) *UsageNow {
	u, ok := s.usage[id]
	if !ok || now.Sub(s.usageAt) > usageFresh {
		return nil
	}
	n := &UsageNow{CPU: u.CPU, Mem: u.Mem, CPUOf: u.CPUOf, MemOf: u.MemOf, IO: u.IO, Leases: u.Leases, At: s.usageAt}
	if u.CPUOf > 0 {
		n.CPUPct = round1(100 * u.CPU / u.CPUOf)
	}
	if u.MemOf > 0 {
		n.MemPct = round1(100 * float64(u.Mem) / float64(u.MemOf))
	}
	return n
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// handleWorkloadHours is one workload's hourly CPU and memory:
// ?source=<cluster>&workload=<namespace/name, or a node's name>&days=<1..400,
// default 1>, oldest first. 400 days is all that's kept.
func (srv *Server) handleWorkloadHours(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source, workload := q.Get("source"), q.Get("workload")
	if source == "" || workload == "" {
		http.Error(w, "source and workload, please", http.StatusBadRequest)
		return
	}
	days, _ := strconv.Atoi(q.Get("days"))
	days = min(max(days, 1), 400)
	hours := []state.WorkloadHour{}
	if srv.db != nil {
		now := time.Now()
		hs, err := srv.db.WorkloadHours(source, workload, now.Add(-time.Duration(days)*24*time.Hour).Truncate(time.Hour), now)
		if err != nil {
			log.Printf("workloads: hours: %v", err)
			http.Error(w, "can't read the hours", http.StatusServiceUnavailable)
			return
		}
		hours = append(hours, hs...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": hours})
}
