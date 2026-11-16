package main

import (
	"fmt"
	"log"
	"math"
	"slices"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

// The demo's past (#39): before anything reports, fleet run -history
// writes the last 30 days into obs-beacond's database, worked out from the
// same clock the live demo runs on (load.go, scenario.go), so the graphs
// have their days and the Alerts page and reports have their incidents:
//
//   - each host's hours: CPU and memory averages and peaks, disks;
//   - each cluster's workloads' hours, an array's I/O and space, an IBM i
//     partition's CPU, a Vault's leases;
//   - the mail monitor's rounds;
//   - hosts' and checks' statuses, from the start;
//   - an incident each time a past problem (pastProblems) came and went,
//     with a note from whoever looked at the stories.

// pastFlaps are the statuses that change with a spell, for the reports'
// availability: an entity takes status while it's on, and after when
// it's over.
var pastFlaps = []struct {
	entity        string
	s             spell
	status, after string
}{
	{"check/web-legacy", spellLegacy502, "warning", "good"},
	{"check/rbl-mx2", spellBlocklisted, "warning", "good"},
	{"svc/sites", spellLegacy502, "warning", "good"},
	{"svc/mail", spellBlocklisted, "warning", "good"},
	{"svc/postgres", spellReplicaBehind, "warning", "good"},
	{"svc/haproxy", spellReplicaBehind, "warning", "good"},
	// One copy of something there are several of is the service degraded,
	// not down (topology.go, redundant parts).
	{"svc/apps", spellAppExits, "warning", "good"},
	{"svc/redis", spellRedisFails, "warning", "good"},
}

// pastFleet is what the history is written for.
type pastFleet struct {
	hosts    []*host
	clusters []*fakeCluster
	vaults   bool // the Vault stand-ins run
	mail     bool // the mail stand-in runs
	checks   []string
}

func writeHistory(path, keyFile string, days int, f pastFleet, now time.Time) error {
	start := time.Now()
	db, err := state.Open(path, keyFile)
	if err != nil {
		return err
	}
	defer db.Close()
	if !db.HistoryStart().IsZero() {
		log.Printf("fleet: %s already has history; left as it is", path)
		return nil
	}
	from := now.Truncate(time.Hour).Add(-time.Duration(days) * 24 * time.Hour)
	to := now.Truncate(time.Hour) // the hour now is obs-beacond's to write

	var hs []state.HostHour
	var statuses []state.Change
	for _, h := range f.hosts {
		name := h.r.Host
		statuses = append(statuses, state.Change{Entity: "host/" + name, At: from, Status: "good"})
		for t := from; t.Before(to); t = t.Add(time.Hour) {
			hs = append(hs, hostHour(h, t))
		}
	}
	for _, id := range f.checks {
		statuses = append(statuses, state.Change{Entity: "check/" + id, At: from, Status: "good"})
	}
	if f.checks != nil { // the demo's services (demoTopology)
		for _, id := range []string{"shop", "api", "sites", "mail", "apps", "haproxy", "postgres", "redis", "nats"} {
			statuses = append(statuses, state.Change{Entity: "svc/" + id, At: from, Status: "good"})
		}
		// Flexible SSL on legacy.example is a lasting warning, and the
		// shop is behind Cloudflare.
		statuses = append(statuses, state.Change{Entity: "svc/cloudflare", At: from, Status: "warning"},
			state.Change{Entity: "svc/shop", At: from.Add(time.Second), Status: "warning"})
		for _, fl := range pastFlaps {
			s := fl.s
			for _, o := range s.between(from, now) {
				if o.end.Before(now) {
					statuses = append(statuses, state.Change{Entity: fl.entity, At: o.start.Add(20 * time.Second), Status: fl.status},
						state.Change{Entity: fl.entity, At: o.end.Add(20 * time.Second), Status: fl.after})
				}
			}
		}
		slices.SortStableFunc(statuses, func(a, b state.Change) int { return a.At.Compare(b.At) })
	}
	if err := db.PutHostHours(hs); err != nil {
		return fmt.Errorf("host hours: %w", err)
	}
	if err := db.LogStatuses(statuses); err != nil {
		return fmt.Errorf("statuses: %w", err)
	}

	nw := 0
	// Thousands of controllers and VMs get a week: a month of each is most
	// of the database, and the demo's state is a 256 MB tmpfs.
	few := to.Add(-time.Duration(min(days, 7)) * 24 * time.Hour)
	for _, c := range f.clusters {
		ws := clusterHours(c, from, few, to)
		if err := db.PutWorkloadHours(ws); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		nw += len(ws)
	}
	if f.vaults {
		var ws []state.WorkloadHour
		for name, base := range vaultLeaseBase {
			for t := from; t.Before(to); t = t.Add(time.Hour) {
				avg, top := 0.0, 0.0
				for m := range 4 {
					l := float64(vaultLeases(base, t.Add(time.Duration(m)*15*time.Minute)))
					avg += l / 4
					top = max(top, l)
				}
				ws = append(ws, state.WorkloadHour{Source: report.SourceKey("obs-beacond", name), Workload: name, Hour: t, Kind: report.KindCluster,
					Leases: math.Round(avg), LeasesMax: top})
			}
		}
		if err := db.PutWorkloadHours(ws); err != nil {
			return fmt.Errorf("vault: %w", err)
		}
		nw += len(ws)
	}

	var ms []state.MailRound
	if f.mail {
		for t := from; t.Before(now.Add(-time.Minute)); t = t.Add(time.Minute) {
			k := int(t.Unix() / 60)
			ms = append(ms, state.MailRound{At: t, Seconds: 1.5 + float64(k*7919%4500)/1000, Queue: 3 + k%6})
		}
		if err := db.PutMailRounds(ms); err != nil {
			return fmt.Errorf("mail rounds: %w", err)
		}
	}

	n, err := writePastIncidents(db, from, now)
	if err != nil {
		return fmt.Errorf("incidents: %w", err)
	}
	log.Printf("fleet: wrote %d days of history in %s: %d host hours, %d workload hours, %d mail rounds, %d incidents",
		days, time.Since(start).Round(time.Millisecond), len(hs), nw, len(ms), n)
	return nil
}

// hostHour is one host's hour, from a reading every five minutes, as
// obs-beacond's own (from every report) would roughly be.
func hostHour(h *host, t time.Time) state.HostHour {
	name := h.r.Host
	out := state.HostHour{Host: name, Hour: t}
	const n = 12
	for k := range n {
		at := t.Add(time.Duration(k)*5*time.Minute + 2*time.Minute)
		c, m := cpuNow(name, at), memAt(name, at)
		out.CPU += c / n
		out.Mem += m / n
		out.CPUMax = max(out.CPUMax, c*(1.1+0.3*seed(name, at.Format("15"))))
		out.MemMax = max(out.MemMax, m+1.5)
	}
	out.CPU, out.CPUMax = round1(out.CPU), round1(min(out.CPUMax, 100))
	out.Mem, out.MemMax = round1(out.Mem), round1(min(out.MemMax, 100))
	end := t.Add(time.Hour - time.Minute)
	for i, d := range h.r.Disks {
		out.Disks = append(out.Disks, state.DiskSample{Mount: d.Mount, Used: uint64(float64(d.Size) * diskAt(name, i, end)), Total: d.Size})
	}
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// clusterHours is a cluster's workloads' hours, from two readings an hour,
// keyed the way obs-beacond keeps them (its historyKey): from from for
// the cluster itself and its nodes, from few for the rest.
func clusterHours(c *fakeCluster, from, few, to time.Time) []state.WorkloadHour {
	if c.noUsage && c.usageFn == nil {
		return nil
	}
	src := report.SourceKey(c.name, c.source)
	c.mu.Lock()
	names := map[string]report.Workload{}
	for id, r := range c.recs {
		names[id] = report.Workload{Namespace: r.Namespace, Name: r.Name, Kind: r.Kind}
	}
	c.mu.Unlock()
	var out []state.WorkloadHour
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		sum := map[string]*state.WorkloadHour{}
		const n = 2
		for k := range n {
			at := t.Add(time.Duration(k)*30*time.Minute + 10*time.Minute)
			us := c.usageFn
			if us == nil {
				us = c.usage
			}
			for _, u := range us(at) {
				r, ok := names[u.ID]
				if !ok || t.Before(few) && r.Kind != report.KindNode && r.Kind != report.KindCluster {
					continue
				}
				key := r.Name
				if r.Namespace != "" {
					key = r.Namespace + "/" + r.Name
				}
				h := sum[key]
				if h == nil {
					h = &state.WorkloadHour{Source: src, Workload: key, Hour: t, Kind: r.Kind}
					sum[key] = h
				}
				h.CPU += u.CPU / n
				h.Mem += float64(u.Mem) / n
				h.CPUMax, h.MemMax = max(h.CPUMax, u.CPU), max(h.MemMax, float64(u.Mem))
				h.CPUOf, h.MemOf = u.CPUOf, float64(u.MemOf)
				if io := u.IO; io != nil {
					h.ReadOps += io.ReadOps / n
					h.WriteOps += io.WriteOps / n
					h.ReadUs += io.ReadUs / n
					h.WriteUs += io.WriteUs / n
					h.ReadBps += io.ReadBps / n
					h.WriteBps += io.WriteBps / n
					h.ReadUsMax, h.WriteUsMax = max(h.ReadUsMax, io.ReadUs*1.4), max(h.WriteUsMax, io.WriteUs*1.4)
					h.Used, h.UsedOf = float64(io.Used), float64(io.UsedOf)
				}
			}
		}
		for _, h := range sum {
			out = append(out, *h)
		}
	}
	return out
}

// writePastIncidents writes an incident each time a past problem came and
// went in [from, now), numbered in order from B-0001, and tells
// obs-beacond where the numbers carry on.
func writePastIncidents(db *state.DB, from, now time.Time) (int, error) {
	type occ struct {
		p          past
		began, end time.Time
		first      bool // the first of its kind on its day: it gets the note
	}
	var all []occ
	for _, p := range pastProblems {
		if p.s.every == 0 {
			continue
		}
		day := ""
		for _, o := range p.s.between(from, now) {
			if !o.end.Before(now) {
				continue // still going: the live demo opens it
			}
			d := o.start.UTC().Format("2006-01-02")
			all = append(all, occ{p, o.start, o.end, d != day})
			day = d
		}
	}
	slices.SortStableFunc(all, func(a, b occ) int { return a.began.Compare(b.began) })
	for k, o := range all {
		p := o.p
		// Seen within a reading or two, told after alerts.for (2m), and
		// resolved within a minute of its end.
		opened := o.began.Add(time.Duration(10+seedInt(p.key, o.began, 40)) * time.Second)
		fired := opened.Add(2 * time.Minute)
		resolved := o.end.Add(time.Duration(15+seedInt(p.key+" end", o.began, 45)) * time.Second)
		inc := state.Incident{ID: fmt.Sprintf("B-%04d", k+1), Key: p.key, Severity: p.sev, Title: p.title, Detail: p.detail,
			Host: p.host, Href: p.href, Opened: opened, Fired: fired, Notified: fired, Resolved: resolved}
		if p.sev == "critical" && o.end.Sub(o.began) > 10*time.Minute {
			inc.AckBy, inc.AckAt = "On call", fired.Add(time.Duration(2+seedInt(p.key+" ack", o.began, 6))*time.Minute)
		}
		if err := db.PutIncident(inc); err != nil {
			return 0, err
		}
		if p.note != "" && o.first {
			if err := db.SetNote(inc.ID, state.Note{Text: p.note, By: "On call", At: resolved.Add(20 * time.Minute)}); err != nil {
				return 0, err
			}
		}
	}
	return len(all), db.SetMeta("incident_seq", fmt.Sprint(len(all)))
}

// seedInt is 0 … n-1, fixed for a key and a moment.
func seedInt(key string, t time.Time, n int) int {
	return int((jitter(key, t) + 1) / 2 * float64(n-1))
}
