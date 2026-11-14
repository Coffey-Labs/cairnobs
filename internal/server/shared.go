package server

// Running as several copies on one PostgreSQL database (docs/scale-and-
// availability.md, Sharing live state). Each copy keeps what it shows in
// memory, as a single obs-beacond does; what one copy changes it writes to
// the database and announces on the bus, and the others read it back:
//
//	host <name>                  a report came in: hosts_live has it
//	points <name>\t<c m rx tx>   a queued report's numbers, for the sparklines
//	setting <section>            a section edited in the UI
//	joined                       a host joined or was removed
//	session <id>                 a session ended
//	incidents                    the evaluator changed incidents
//	ack <id>                     an incident was acknowledged on a copy that
//	                             isn't the evaluator; the evaluator tells the
//	                             channels that follow incidents
//	lease                        the evaluator gave the lease up
//	workloads <cluster>          a cluster agent's batch: workloads has it
//	reading <name>               the evaluator polled Cloudflare, Patroni
//	                             or the mail round trip (readings.go)
//
// Notices carry names, not data, so one that's missed costs only a delay:
// every copy also catches up when it (re)connects and every resyncEvery.
// On SQLite none of this runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	noticeHost      = "host"
	noticePoints    = "points"
	noticeSetting   = "setting"
	noticeJoined    = "joined"
	noticeSession   = "session"
	noticeIncidents = "incidents"
	noticeAck       = "ack"
	noticeLease     = "lease"
	noticeReading   = "reading"
	noticeWorkloads = "workloads"

	resyncEvery = 30 * time.Second
)

// shared says whether other copies of obs-beacond share this one's database.
func (srv *Server) shared() bool { return srv.db != nil && srv.db.Shared() }

// publish tells the other copies something changed.
func (srv *Server) publish(kind, name string) {
	if !srv.shared() {
		return
	}
	if err := srv.db.Notify(srv.copyID + "\t" + kind + "\t" + name); err != nil {
		log.Printf("shared: can't announce %s %s: %v", kind, name, err)
	}
}

// follow listens to the other copies until ctx ends. caughtUp closes once
// this copy has heard everything so far (at once on SQLite): what step 5's
// readiness probe will wait for.
func (srv *Server) follow(ctx context.Context) {
	var once sync.Once
	up := func() { once.Do(func() { close(srv.caughtUp) }) }
	if !srv.shared() {
		up()
		return
	}
	go func() {
		t := time.NewTicker(resyncEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				srv.resync()
			}
		}
	}()
	srv.db.Listen(ctx, func() { srv.resync(); up() }, srv.heard)
}

func (srv *Server) heard(payload string) {
	from, rest, _ := strings.Cut(payload, "\t")
	if from == srv.copyID {
		return
	}
	kind, name, _ := strings.Cut(rest, "\t")
	switch kind {
	case noticeHost:
		srv.refreshHost(name)
	case noticePoints:
		agent, nums, _ := strings.Cut(name, "\t")
		var c, m, rx, tx float64
		if _, err := fmt.Sscan(nums, &c, &m, &rx, &tx); err == nil && srv.knownAgent(agent) {
			srv.store.Points(agent, c, m, rx, tx)
		}
	case noticeSetting:
		srv.reloadSection(name, true)
	case noticeJoined:
		srv.reloadJoined()
	case noticeSession:
		srv.sessions.forget(name)
	case noticeIncidents:
		if !srv.leading() {
			srv.alerts.load()
		}
	case noticeAck:
		srv.alerts.acknowledged(name, srv.leading())
	case noticeLease:
		srv.released()
	case noticeReading:
		srv.refreshReading(name)
	case noticeWorkloads:
		srv.syncWorkloads(false)
	}
}

// resync catches up on everything another copy may have changed.
func (srv *Server) resync() {
	srv.reloadJoined()
	srv.resyncHosts()
	for _, s := range []string{sectionChecks, sectionMaintenance, sectionChannels, sectionExpected, sectionMaps} {
		srv.reloadSection(s, false)
	}
	if ids, err := srv.db.SessionIDs(); err == nil {
		srv.sessions.keepOnly(ids)
	}
	if !srv.leading() {
		srv.alerts.load()
	}
	srv.resyncReadings()
	srv.syncWorkloads(false)
}

// resyncHosts reads every host whose latest report is newer than this
// copy's.
func (srv *Server) resyncHosts() {
	times, err := srv.db.LiveTimes()
	if err != nil {
		log.Printf("shared: hosts: %v", err)
		return
	}
	for h, at := range times {
		if at.After(srv.store.Seen(h)) {
			srv.refreshHost(h)
		}
	}
}

// refreshHost reads one host's latest report from the database.
func (srv *Server) refreshHost(name string) {
	if !srv.knownAgent(name) {
		return
	}
	b, at, err := srv.db.Live(name)
	if err != nil {
		if err != state.ErrNotFound {
			log.Printf("shared: host %s: %v", name, err)
		}
		return
	}
	var rep report.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		log.Printf("shared: host %s: %v", name, err)
		return
	}
	srv.store.PutIfNewer(name, rep, at)
}

// shareReport writes a report this copy took for the others.
func (srv *Server) shareReport(agent string, body []byte, rep report.Report, queued bool, at time.Time) {
	if !srv.shared() {
		return
	}
	if queued {
		srv.publish(noticePoints, fmt.Sprintf("%s\t%g %g %g %g", agent, rep.CPU, memPct(rep.Mem), rep.Net.RxBps, rep.Net.TxBps))
		return
	}
	if err := srv.db.PutLive(agent, body, at); err != nil {
		log.Printf("shared: can't store %s's report: %v", agent, err)
		return
	}
	srv.publish(noticeHost, agent)
}

// reloadSection reads a section edited in the UI, when it changed since
// this copy last read it (or always, when force). A section that doesn't
// parse is logged and the one in force kept.
func (srv *Server) reloadSection(section string, force bool) {
	s, ok, err := srv.db.Setting(section)
	if err != nil {
		log.Printf("shared: %s: %v", section, err)
		return
	}
	srv.sectionsMu.Lock()
	seen := srv.sectionSeen[section]
	if !force && seen.Equal(s.Updated) {
		srv.sectionsMu.Unlock()
		return
	}
	srv.sectionSeen[section] = s.Updated
	srv.sectionsMu.Unlock()
	switch section {
	case sectionChecks:
		srv.checks.mu.Lock()
		err = srv.loadChecksSetting()
		srv.checks.mu.Unlock()
	case sectionMaintenance:
		srv.maint.mu.Lock()
		err = srv.loadMaintenanceSetting()
		srv.maint.mu.Unlock()
	case sectionChannels:
		srv.uiChans.mu.Lock()
		err = srv.loadChannels()
		srv.uiChans.mu.Unlock()
	case sectionExpected:
		srv.expMu.Lock()
		err = srv.loadExpectedSetting()
		srv.expMu.Unlock()
	case sectionMaps:
		err = srv.loadMapFixes()
	default:
		return
	}
	if err != nil {
		log.Printf("shared: %s as another copy saved it: %v; keeping the one in force", section, err)
	} else if ok && !seen.IsZero() { // not the first read, at start
		log.Printf("shared: %s reloaded (saved by %s)", section, s.UpdatedBy)
	}
}

// reloadJoined reads the joined agents again: another copy may have let one
// join or removed one.
func (srv *Server) reloadJoined() {
	list, err := srv.db.JoinedAgents()
	if err != nil {
		log.Printf("shared: joined agents: %v", err)
		return
	}
	now := map[string]state.JoinedAgent{}
	for _, a := range list {
		if !srv.inConfig(a.Name) {
			now[a.Name] = a
		}
	}
	srv.joinedMu.Lock()
	old := srv.joined
	srv.joined = now
	srv.joinedMu.Unlock()
	// Only hosts are in the store: a cluster agent isn't a host.
	for n, a := range now {
		if _, ok := old[n]; !ok && !a.Cluster {
			srv.store.Add(n)
		}
	}
	for n, a := range old {
		if _, ok := now[n]; !ok && !a.Cluster {
			srv.store.Remove(n)
		}
	}
	srv.syncWorkloads(true) // a cluster that joined or went shows (or stops showing) at once
}
