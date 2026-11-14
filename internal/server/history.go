package server

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// keepHistory is how long reports can look back.
const keepHistory = 400 * 24 * time.Hour

// recorder writes what reports need, once a minute: each service's,
// host's and check's status when it changes, an hourly sample per host,
// and every mail round. While obs-beacond is down nothing is known, and the
// reports say so rather than assume the last status held.
type recorder struct {
	last     map[string]string // entity → last status written
	hour     time.Time         // the hour being gathered
	samples  map[string]*hourSum
	wl       map[string]*state.WorkloadHour // source + workload → the hour so far (sums until written)
	wlN      map[string]int                 // readings in each
	wlSeen   map[string]time.Time           // each cluster's reading last counted
	mailSeen time.Time
	pruned   time.Time
}

type hourSum struct {
	n           int
	cpu, cpuMax float64
	mem, memMax float64
	disks       []state.DiskSample
}

func (srv *Server) recordHistory(ctx context.Context) {
	if srv.db == nil {
		return
	}
	rec := &recorder{samples: map[string]*hourSum{}, wl: map[string]*state.WorkloadHour{}, wlN: map[string]int{}, wlSeen: map[string]time.Time{}}
	var err error
	if rec.last, err = srv.db.LatestStatuses(); err != nil {
		log.Printf("history: %v", err)
		rec.last = map[string]string{}
	}
	// The gap while obs-beacond was down: from a minute after its last heartbeat.
	if hb, err := strconv.ParseInt(srv.db.Meta("history_heartbeat"), 10, 64); err == nil {
		gap := time.Unix(hb, 0).Add(time.Minute)
		for e, s := range rec.last {
			if s != "unknown" {
				_ = srv.db.LogStatus(e, "unknown", gap)
				rec.last[e] = "unknown"
			}
		}
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// Like alerts: nothing is judged until every agent has had time to report.
			if now.Sub(srv.started) >= srv.cfg.StaleAfter {
				srv.recordOnce(rec, now)
			}
		}
	}
}

// statuses is every entity's status now: services on the map, hosts, checks.
func (srv *Server) statuses(now time.Time) map[string]string {
	out := map[string]string{}
	hosts, _ := srv.view(now)
	for _, h := range hosts {
		s := h.Status
		if h.Silent {
			s = "critical"
		}
		out["host/"+h.Name] = s
	}
	checks, _ := srv.judgeChecks(now)
	for _, c := range checks {
		out["check/"+c.ID] = c.Status
	}
	if v := srv.topologyView(now); v["configured"] != false {
		if svcs, ok := v["services"].([]ServiceView); ok {
			for _, s := range svcs {
				out["svc/"+s.ID] = s.Status
			}
		}
	}
	return out
}

func (srv *Server) recordOnce(rec *recorder, now time.Time) {
	for e, s := range srv.statuses(now) {
		if rec.last[e] != s {
			if err := srv.db.LogStatus(e, s, now); err != nil {
				log.Printf("history: %v", err)
				return
			}
			rec.last[e] = s
		}
	}
	_ = srv.db.SetMeta("history_heartbeat", strconv.FormatInt(now.Unix(), 10))

	// Hosts, hourly.
	hour := now.Truncate(time.Hour)
	if !rec.hour.IsZero() && hour.After(rec.hour) {
		for host, s := range rec.samples {
			if s.n == 0 {
				continue
			}
			h := state.HostHour{Host: host, Hour: rec.hour, CPU: s.cpu / float64(s.n), CPUMax: s.cpuMax,
				Mem: s.mem / float64(s.n), MemMax: s.memMax, Disks: s.disks}
			if err := srv.db.PutHostHour(h); err != nil {
				log.Printf("history: %v", err)
			}
		}
		rec.samples = map[string]*hourSum{}
		srv.writeWorkloadHours(rec)
	}
	rec.hour = hour
	srv.sampleWorkloads(rec, hour, now)
	hosts, _ := srv.view(now)
	for _, h := range hosts {
		if h.Silent || h.LastSeen == nil {
			continue
		}
		s := rec.samples[h.Name]
		if s == nil {
			s = &hourSum{}
			rec.samples[h.Name] = s
		}
		s.n++
		s.cpu += h.CPU
		s.mem += h.MemPct
		s.cpuMax = max(s.cpuMax, h.CPU)
		s.memMax = max(s.memMax, h.MemPct)
		s.disks = s.disks[:0]
		for _, d := range h.Disks {
			s.disks = append(s.disks, state.DiskSample{Mount: d.Mount, Used: d.Used, Total: d.Size})
		}
	}

	// Mail rounds.
	if srv.mail != nil {
		m := srv.mail.Snapshot()
		if !m.Checked.IsZero() && m.Checked.After(rec.mailSeen) && !m.Trip.At.IsZero() {
			r := state.MailRound{At: m.Trip.At, Seconds: -1, Queue: -1}
			if m.Trip.OK {
				r.Seconds = m.Trip.Seconds
			}
			if m.Queue.Read {
				r.Queue = m.Queue.Total
			}
			if err := srv.db.PutMailRound(r); err != nil {
				log.Printf("history: %v", err)
			}
			rec.mailSeen = m.Checked
		}
	}

	if now.Sub(rec.pruned) > 24*time.Hour {
		if err := srv.db.Prune(now.Add(-keepHistory)); err != nil {
			log.Printf("history: prune: %v", err)
		}
		rec.pruned = now
	}
}

// sampleWorkloads adds each cluster's newest usage reading to its
// workloads' hour, once per reading.
func (srv *Server) sampleWorkloads(rec *recorder, hour, now time.Time) {
	for _, s := range srv.workloadSnapshot() {
		if s.usage == nil || now.Sub(s.usageAt) > usageFresh || !s.usageAt.After(rec.wlSeen[s.name]) {
			continue
		}
		rec.wlSeen[s.name] = s.usageAt
		for _, r := range s.recs {
			u, ok := s.usage[r.ID]
			if !ok {
				continue
			}
			key := historyKey(r)
			h := rec.wl[s.name+"\x00"+key]
			if h == nil {
				h = &state.WorkloadHour{Source: s.name, Workload: key, Hour: hour, Kind: r.Kind}
				rec.wl[s.name+"\x00"+key] = h
			}
			rec.wlN[s.name+"\x00"+key]++
			h.CPU += u.CPU
			h.Mem += float64(u.Mem)
			h.CPUMax = max(h.CPUMax, u.CPU)
			h.MemMax = max(h.MemMax, float64(u.Mem))
			h.CPUOf, h.MemOf = u.CPUOf, float64(u.MemOf) // the latest ceiling
			if io := u.IO; io != nil {
				h.ReadOps += io.ReadOps
				h.WriteOps += io.WriteOps
				h.ReadUs += io.ReadUs
				h.WriteUs += io.WriteUs
				h.ReadBps += io.ReadBps
				h.WriteBps += io.WriteBps
				h.ReadUsMax = max(h.ReadUsMax, io.ReadUs)
				h.WriteUsMax = max(h.WriteUsMax, io.WriteUs)
				h.Used, h.UsedOf = float64(io.Used), float64(io.UsedOf) // the latest
			}
			if l := u.Leases; l != nil {
				h.Leases += float64(*l)
				h.LeasesMax = max(h.LeasesMax, float64(*l))
			}
		}
	}
}

// writeWorkloadHours writes the hour just ended: averages from the sums.
func (srv *Server) writeWorkloadHours(rec *recorder) {
	hs := make([]state.WorkloadHour, 0, len(rec.wl))
	for k, h := range rec.wl {
		n := float64(rec.wlN[k])
		h.CPU, h.Mem = h.CPU/n, h.Mem/n
		h.ReadOps, h.WriteOps, h.ReadUs, h.WriteUs, h.ReadBps, h.WriteBps = h.ReadOps/n, h.WriteOps/n, h.ReadUs/n, h.WriteUs/n, h.ReadBps/n, h.WriteBps/n
		h.Leases /= n
		hs = append(hs, *h)
	}
	if err := srv.db.PutWorkloadHours(hs); err != nil {
		log.Printf("history: workload hours: %v", err)
	}
	rec.wl, rec.wlN = map[string]*state.WorkloadHour{}, map[string]int{}
}

// historyKey is a workload's name in its history: "namespace/name", or a
// node's name. It outlives the object, so a Deployment made again keeps
// its history.
func historyKey(r report.Workload) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}
