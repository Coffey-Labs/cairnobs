package server

import (
	"sort"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// historyLen points of history are kept per host in memory, an hour at the
// default 15 s interval, for the sparklines. Long-term history goes to
// VictoriaMetrics in a later phase.
const historyLen = 240

type hostState struct {
	name     string
	last     report.Report
	seen     time.Time // when obs-beacond received the last report; zero before the first
	cpu, mem []float64 // oldest first
	rx, tx   []float64
	mp       *report.Map // the latest map sample (agents send one a minute)
	mapAt    time.Time
	certs    *report.CertScan  // what its ports served (sent on a change, and half-hourly)
	files    *report.CertFiles // what its certificate files hold (the same)
	events   *report.Events    // a Windows host's event log (the same, hourly)
}

// Store holds the latest report and short history for each agent.
type Store struct {
	mu    sync.RWMutex
	hosts map[string]*hostState
}

// NewStore starts with every configured agent, so one that has never
// reported still shows up, as waiting.
func NewStore(agents []AgentConfig) *Store {
	s := &Store{hosts: map[string]*hostState{}}
	for _, a := range agents {
		s.hosts[a.Name] = &hostState{name: a.Name}
	}
	return s
}

func push(h []float64, v float64) []float64 {
	if len(h) == historyLen {
		h = append(h[:0:0], h[1:]...)
	}
	return append(h, v)
}

// Add starts watching a new agent: it shows as waiting until it reports.
func (s *Store) Add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosts[name] == nil {
		s.hosts[name] = &hostState{name: name}
	}
}

// Remove forgets an agent.
func (s *Store) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, name)
}

// Put records a report from the named agent.
func (s *Store) Put(agent string, r report.Report, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[agent]
	if h == nil {
		h = &hostState{name: agent}
		s.hosts[agent] = h
	}
	h.last, h.seen = r, now
	h.keepMap(now)
	h.points(r)
}

// keepMap holds on to the newest map sample: reports without one (most of
// them) leave the last in place.
func (h *hostState) keepMap(at time.Time) {
	if h.last.Map != nil {
		h.mp, h.mapAt = h.last.Map, at
		h.last.Map = nil
	}
	if h.last.Certs != nil { // the same for the certificates its ports serve
		h.certs = h.last.Certs
		h.last.Certs = nil
	}
	if h.last.CertFiles != nil {
		h.files = h.last.CertFiles
		h.last.CertFiles = nil
	}
	if h.last.Events != nil {
		h.events = h.last.Events
		h.last.Events = nil
	}
}

// History adds a queued report's numbers to the sparklines and leaves the
// host's state alone.
func (s *Store) History(agent string, r report.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[agent]
	if h == nil {
		h = &hostState{name: agent}
		s.hosts[agent] = h
	}
	h.points(r)
}

// PutIfNewer records a report another copy of obs-beacond took, unless this
// copy already has that one or a later one. It says whether it did.
func (s *Store) PutIfNewer(agent string, r report.Report, received time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[agent]
	if h == nil {
		h = &hostState{name: agent}
		s.hosts[agent] = h
	}
	if !received.After(h.seen) {
		return false
	}
	h.last, h.seen = r, received
	h.keepMap(received)
	h.points(r)
	return true
}

// Seen says when the host's latest report came; zero before the first.
func (s *Store) Seen(agent string) time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if h := s.hosts[agent]; h != nil {
		return h.seen
	}
	return time.Time{}
}

// Points adds one queued report's numbers, as another copy heard them.
func (s *Store) Points(agent string, cpu, mem, rx, tx float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[agent]; h != nil {
		h.cpu, h.mem = push(h.cpu, cpu), push(h.mem, mem)
		h.rx, h.tx = push(h.rx, rx), push(h.tx, tx)
	}
}

func (h *hostState) points(r report.Report) {
	h.cpu = push(h.cpu, r.CPU)
	h.mem = push(h.mem, memPct(r.Mem))
	h.rx = push(h.rx, r.Net.RxBps)
	h.tx = push(h.tx, r.Net.TxBps)
}

// Machines is every host's SMBIOS UUID, as last reported, by host.
func (s *Store) Machines() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for n, h := range s.hosts {
		if h.last.Machine != "" {
			out[n] = h.last.Machine
		}
	}
	return out
}

// hostVMs is one host's libvirt VMs as last reported (docs/cluster-watcher.md,
// step 6). The report's slices are never changed after Put: read only.
type hostVMs struct {
	name  string
	seen  time.Time
	agent string
	at    time.Time // the report's own time: when the usage was read
	vms   *report.VMs
}

// HostVMs is every host that reported libvirt VMs, sorted by name.
func (s *Store) HostVMs() []hostVMs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []hostVMs
	for _, h := range s.hosts {
		if h.last.VMs != nil && !h.seen.IsZero() {
			out = append(out, hostVMs{name: h.name, seen: h.seen, agent: h.last.Agent, at: h.last.Time, vms: h.last.VMs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Snapshot copies every host's state, sorted by name.
func (s *Store) Snapshot() []hostState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]hostState, 0, len(s.hosts))
	for _, h := range s.hosts {
		c := *h
		c.cpu = append([]float64(nil), h.cpu...)
		c.mem = append([]float64(nil), h.mem...)
		c.rx = append([]float64(nil), h.rx...)
		c.tx = append([]float64(nil), h.tx...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func memPct(m report.Mem) float64 {
	if m.Total == 0 {
		return 0
	}
	return 100 * float64(m.Total-min(m.Available, m.Total)) / float64(m.Total)
}
