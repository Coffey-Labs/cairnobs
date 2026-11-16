package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

// What the dashboard's briefing needs beyond the readings (#69, #70):
// when each person last looked and what happened since, every service's
// last 24 hours, and a watch list of the few hosts heading somewhere. The
// slow parts come from history at most every few minutes, not on every
// poll: every open dashboard reads the overview every few seconds.

const (
	visitGap      = 30 * time.Minute // a visit ends after this long without the page open
	sinceDefault  = 12 * time.Hour   // "since you last looked" with no last visit on record
	stripStep     = 30 * time.Minute // one bar of a service's strip
	stripCells    = 48               // a day of them
	todayEvents   = 8                // events listed; the counts cover all
	svcRefresh    = time.Minute
	svcMonthEvery = 10 * time.Minute
	watchRefresh  = 10 * time.Minute
	watchDays     = 7
	watchMax      = 5
)

// visitBook keeps each person's previous visit. A visit ends after
// visitGap without the page open; the end of the last one is what "since
// you last looked" counts from. Written to the database at most once a
// minute a person, so it survives a restart.
type visitBook struct {
	mu sync.Mutex
	m  map[string]*visitMark
}

type visitMark struct{ prev, last, saved time.Time }

// visited notes that user has the dashboard open now and returns the end of
// their previous visit: zero when there's none on record.
func (srv *Server) visited(user string, now time.Time) time.Time {
	b := &srv.visits
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*visitMark{}
	}
	v := b.m[user]
	if v == nil {
		v = &visitMark{}
		if srv.db != nil {
			v.prev, v.last = parseVisit(srv.db.Meta("visit/" + user))
			v.saved = v.last
		}
		b.m[user] = v
	}
	if !v.last.IsZero() && now.Sub(v.last) > visitGap {
		v.prev = v.last
	}
	v.last = now
	if srv.db != nil && now.Sub(v.saved) >= time.Minute {
		v.saved = now
		_ = srv.db.SetMeta("visit/"+user, fmt.Sprintf("%d %d", unixOrZero(v.prev), unixOrZero(v.last)))
	}
	return v.prev
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func parseVisit(s string) (prev, last time.Time) {
	a, b, _ := strings.Cut(s, " ")
	at := func(x string) time.Time {
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	return at(a), at(b)
}

// Today is what happened since the person last looked.
type Today struct {
	Since     time.Time `json:"since"`
	LastVisit bool      `json:"last_visit"` // Since is their previous visit, not the default 12 hours
	// Events are the newest incidents that opened or closed since, newest first.
	Events      []TodayEvent `json:"events"`
	Opened      int          `json:"opened"`
	Resolved    int          `json:"resolved"`
	FixedItself int          `json:"fixed_itself"` // resolved with nobody having acknowledged it
	Told        int          `json:"told"`         // alerts that went out to people
}

// TodayEvent is one incident in Today.
type TodayEvent struct {
	ID       string     `json:"id"`
	Severity string     `json:"severity"`
	Title    string     `json:"title"`
	Host     string     `json:"host,omitempty"`
	Href     string     `json:"href,omitempty"`
	At       time.Time  `json:"at"` // when it opened, or closed when it opened before Since
	Opened   time.Time  `json:"opened"`
	Resolved *time.Time `json:"resolved,omitempty"`
	AckBy    string     `json:"ack_by,omitempty"`
	Told     bool       `json:"told"`
}

// today sums up the incidents that opened or closed since since.
func today(open, recent []Incident, since time.Time) Today {
	t := Today{Since: since, Events: []TodayEvent{}}
	for _, inc := range append(append([]Incident{}, open...), recent...) {
		opened := !inc.Opened.Before(since)
		closed := inc.Resolved != nil && !inc.Resolved.Before(since)
		if !opened && !closed {
			continue
		}
		e := TodayEvent{ID: inc.ID, Severity: inc.Severity, Title: inc.Title, Host: inc.Host, Href: inc.Href, At: inc.Opened, Opened: inc.Opened, Resolved: inc.Resolved}
		if !opened {
			e.At = *inc.Resolved
		}
		if inc.Ack != nil {
			e.AckBy = inc.Ack.By
		}
		if inc.Fired != nil && !inc.Fired.Before(since) {
			e.Told = true
			t.Told++
		}
		if opened {
			t.Opened++
		}
		if closed {
			t.Resolved++
			if inc.Ack == nil {
				t.FixedItself++
			}
		}
		t.Events = append(t.Events, e)
	}
	sort.SliceStable(t.Events, func(i, j int) bool {
		if !t.Events[i].At.Equal(t.Events[j].At) {
			return t.Events[i].At.After(t.Events[j].At)
		}
		return idLess(t.Events[j].ID, t.Events[i].ID)
	})
	if len(t.Events) > todayEvents {
		t.Events = t.Events[:todayEvents]
	}
	return t
}

// todayFor is Today for the person asking. A demo visitor shares one
// sign-in with everyone else, so they get the last 12 hours.
func (srv *Server) todayFor(r *http.Request, now time.Time) Today {
	since, last := now.Add(-sinceDefault), false
	if _, s, ok := srv.sessions.get(r); ok && !s.demo() && s.User != "" {
		if prev := srv.visited(s.User, now); !prev.IsZero() {
			since, last = prev, true
		}
	}
	open, recent := srv.alerts.snapshot()
	t := today(open, recent, since)
	t.LastVisit = last
	return t
}

// ServiceHistory is a service's recent past.
type ServiceHistory struct {
	// Cells are the last 24 hours, half an hour each, oldest first: the
	// worst status in each, or "" when nothing is known.
	Cells []string `json:"cells"`
	Pct24 float64  `json:"pct_24h"` // up, of the time known; -1 when nothing is
	Pct30 float64  `json:"pct_30d"`
}

// strips gives each entity's worst status in each of n steps from from.
// changes are oldest first, with each entity's status as from began.
func strips(changes []state.Change, from time.Time, n int, step time.Duration) map[string][]string {
	by := map[string][]state.Change{}
	for _, c := range changes {
		by[c.Entity] = append(by[c.Entity], c)
	}
	out := map[string][]string{}
	for e, cs := range by {
		cells := make([]string, n)
		cur, j := "", 0
		for i := range n {
			t0 := from.Add(time.Duration(i) * step)
			t1 := t0.Add(step)
			for j < len(cs) && !cs[j].At.After(t0) {
				cur = cs[j].Status
				j++
			}
			w := cur
			for k := j; k < len(cs) && cs[k].At.Before(t1); k++ {
				w = cellWorst(w, cs[k].Status)
			}
			if w == "unknown" {
				w = ""
			}
			cells[i] = w
		}
		out[e] = cells
	}
	return out
}

// cellWorst is the worse of two statuses, where not knowing loses to
// anything known: a half hour obs-beacond was down for part of shows what
// it saw in the rest.
func cellWorst(a, b string) string {
	if a == "" || a == "unknown" {
		return b
	}
	if b == "" || b == "unknown" {
		return a
	}
	return worst(a, b)
}

// trendCache holds what comes from history, refreshed now and then.
type trendCache struct {
	mu       sync.Mutex
	svcAt    time.Time
	monthAt  time.Time
	svc      map[string]ServiceHistory // by service id
	month    map[string]float64
	watchAt  time.Time
	filling  []WatchDisk
	usual    map[string]*[24]usualHour // host → its hours of the day over the past week
	hoursErr error
}

type usualHour struct {
	n        int
	cpu, mem float64
}

// serviceHistory is every service's ServiceHistory, by id.
func (srv *Server) serviceHistory(now time.Time) map[string]ServiceHistory {
	c := &srv.trends
	c.mu.Lock()
	defer c.mu.Unlock()
	if srv.db == nil {
		return map[string]ServiceHistory{}
	}
	if c.svc != nil && now.Sub(c.svcAt) < svcRefresh {
		return c.svc
	}
	if c.month == nil || now.Sub(c.monthAt) >= svcMonthEvery {
		c.month = map[string]float64{}
		if ch, err := srv.db.StatusChangesOf("svc/", now.Add(-30*24*time.Hour), now); err == nil {
			for e, u := range integrate(ch, now.Add(-30*24*time.Hour), now) {
				c.month[e] = u.Pct
			}
			c.monthAt = now
		}
	}
	from := now.Truncate(stripStep).Add(-(stripCells - 1) * stripStep) // the last cell is the half hour now
	ch, err := srv.db.StatusChangesOf("svc/", from, now)
	if err != nil {
		return map[string]ServiceHistory{}
	}
	day := integrate(ch, now.Add(-24*time.Hour), now)
	out := map[string]ServiceHistory{}
	for e, cells := range strips(ch, from, stripCells, stripStep) {
		id := strings.TrimPrefix(e, "svc/")
		h := ServiceHistory{Cells: cells, Pct24: -1, Pct30: -1}
		if u := day[e]; u != nil {
			h.Pct24 = u.Pct
		}
		if p, ok := c.month[e]; ok {
			h.Pct30 = p
		}
		out[id] = h
	}
	c.svc, c.svcAt = out, now
	return out
}

// ServiceBrief is a service as the dashboard shows it.
type ServiceBrief struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Tier    string         `json:"tier"`
	Status  string         `json:"status"`
	Word    string         `json:"word"`
	History ServiceHistory `json:"history"`
}

// ServiceDetail is a service as the Services page shows it (#71).
type ServiceDetail struct {
	ServiceView
	NeededBy []string       `json:"needed_by"` // the services that need it
	History  ServiceHistory `json:"history"`
}

func emptyHistory() ServiceHistory {
	return ServiceHistory{Cells: make([]string, stripCells), Pct24: -1, Pct30: -1}
}

func (srv *Server) serviceBriefs(now time.Time) []ServiceBrief {
	views, ok := srv.serviceStates(now)
	out := []ServiceBrief{}
	if !ok {
		return out
	}
	hist := srv.serviceHistory(now)
	for _, v := range views {
		h, ok := hist[v.ID]
		if !ok {
			h = emptyHistory()
		}
		out = append(out, ServiceBrief{ID: v.ID, Name: v.Name, Tier: v.Tier, Status: v.Status, Word: v.Word, History: h})
	}
	return out
}

func (srv *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	views, ok := srv.serviceStates(now)
	out := []ServiceDetail{}
	if ok {
		hist := srv.serviceHistory(now)
		by := map[string][]string{}
		for _, v := range views {
			for _, n := range v.Needs {
				by[n] = append(by[n], v.ID)
			}
		}
		for _, v := range views {
			h, ok := hist[v.ID]
			if !ok {
				h = emptyHistory()
			}
			d := ServiceDetail{ServiceView: v, NeededBy: nonNilSlice(by[v.ID]), History: h}
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out, "topology": ok})
}

// Watch is the few hosts heading somewhere, in place of gauges.
type Watch struct {
	Filling    []WatchDisk  `json:"filling"`     // disks that fill within 30 days at the past week's rate
	Busier     []WatchBusy  `json:"busier"`      // busier than usual for the hour
	Restarted  []WatchStart `json:"restarted"`   // up less than a day, most recent first
	RestartedN int          `json:"restarted_n"` // all of them; Restarted is the first few
}

// WatchDisk is a disk filling up.
type WatchDisk struct {
	Host     string  `json:"host"`
	Mount    string  `json:"mount"`
	Pct      float64 `json:"pct"`
	FullDays float64 `json:"full_days"`
}

// WatchBusy is a host busier than it usually is at this hour.
type WatchBusy struct {
	Host  string  `json:"host"`
	What  string  `json:"what"` // cpu or memory
	Now   float64 `json:"now"`
	Usual float64 `json:"usual"`
}

// WatchStart is a host that started in the last day.
type WatchStart struct {
	Host string  `json:"host"`
	UpS  float64 `json:"up_s"`
}

// watch is the watch list for hosts as they are now.
func (srv *Server) watch(hosts []HostView, now time.Time) Watch {
	w := Watch{Filling: []WatchDisk{}, Busier: []WatchBusy{}, Restarted: []WatchStart{}}
	live := map[string]bool{}
	for _, h := range hosts {
		live[h.Name] = true
	}
	filling, usual := srv.weekTrends(now)
	for _, d := range filling {
		if live[d.Host] && len(w.Filling) < watchMax {
			w.Filling = append(w.Filling, d)
		}
	}
	w.Busier = busier(hosts, usual, now)
	w.Restarted, w.RestartedN = restarted(hosts)
	return w
}

// weekTrends is the past week's disks that are filling, soonest first, and
// each host's usual CPU and memory by hour of the day.
func (srv *Server) weekTrends(now time.Time) ([]WatchDisk, map[string]*[24]usualHour) {
	c := &srv.trends
	c.mu.Lock()
	defer c.mu.Unlock()
	if srv.db == nil {
		return nil, nil
	}
	if c.usual != nil && now.Sub(c.watchAt) < watchRefresh {
		return c.filling, c.usual
	}
	c.watchAt = now // a failing read isn't retried on every poll
	hours, err := srv.db.HostHours(now.Add(-watchDays*24*time.Hour), now)
	if err != nil {
		c.hoursErr = err
		return c.filling, c.usual
	}
	c.filling, c.usual = fillingDisks(hours), usualHours(hours)
	return c.filling, c.usual
}

func fillingDisks(hours []state.HostHour) []WatchDisk {
	disks, _ := capacity(hours, time.UTC)
	out := []WatchDisk{}
	for _, d := range disks {
		if d.FullDays != nil && *d.FullDays <= 30 && d.Pct >= 50 {
			out = append(out, WatchDisk{Host: d.Host, Mount: d.Mount, Pct: d.Pct, FullDays: *d.FullDays})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullDays < out[j].FullDays })
	return out
}

func usualHours(hours []state.HostHour) map[string]*[24]usualHour {
	out := map[string]*[24]usualHour{}
	for _, h := range hours {
		u := out[h.Host]
		if u == nil {
			u = &[24]usualHour{}
			out[h.Host] = u
		}
		k := h.Hour.UTC().Hour()
		u[k].n++
		u[k].cpu += h.CPU
		u[k].mem += h.Mem
	}
	return out
}

// busier is the hosts noticeably busier than they usually are at this
// hour of the day: CPU at least twice usual and 15 points up (and 25% at
// least), or memory 15 points up and 60% at least. Three days of that hour
// before anything is usual.
func busier(hosts []HostView, usual map[string]*[24]usualHour, now time.Time) []WatchBusy {
	out := []WatchBusy{}
	k := now.UTC().Hour()
	for _, h := range hosts {
		u := usual[h.Name]
		if h.Silent || u == nil || u[k].n < 3 {
			continue
		}
		cpu, mem := u[k].cpu/float64(u[k].n), u[k].mem/float64(u[k].n)
		if h.CPU >= 25 && h.CPU >= 2*cpu && h.CPU-cpu >= 15 {
			out = append(out, WatchBusy{Host: h.Name, What: "cpu", Now: h.CPU, Usual: cpu})
		}
		if h.MemPct >= 60 && h.MemPct-mem >= 15 {
			out = append(out, WatchBusy{Host: h.Name, What: "memory", Now: h.MemPct, Usual: mem})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Now-out[i].Usual, out[j].Now-out[j].Usual
		if di != dj {
			return di > dj
		}
		return out[i].Host < out[j].Host
	})
	if len(out) > watchMax {
		out = out[:watchMax]
	}
	return out
}

// restarted is the hosts up less than a day, most recent first, and how
// many there are.
func restarted(hosts []HostView) ([]WatchStart, int) {
	out := []WatchStart{}
	for _, h := range hosts {
		if !h.Silent && h.Uptime > 0 && h.Uptime < 24*3600 {
			out = append(out, WatchStart{Host: h.Name, UpS: h.Uptime})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpS != out[j].UpS {
			return out[i].UpS < out[j].UpS
		}
		return out[i].Host < out[j].Host
	})
	n := len(out)
	if n > watchMax {
		out = out[:watchMax]
	}
	return out, n
}

// AttnIncident is the open incident behind an attention item, so the
// dashboard can offer "I'm on it" (the acknowledgement).
type AttnIncident struct {
	ID     string     `json:"id"`
	Opened time.Time  `json:"opened"`
	Fired  *time.Time `json:"fired"`
	Ack    *Ack       `json:"ack"`
}

// withIncidents names each attention item's open incident, by key.
func withIncidents(attn []Attention, open []Incident) []Attention {
	by := map[string]Incident{}
	for _, inc := range open {
		by[inc.Key] = inc
	}
	for i, a := range attn {
		if inc, ok := by[a.Key]; ok && a.Key != "" {
			attn[i].Incident = &AttnIncident{ID: inc.ID, Opened: inc.Opened, Fired: inc.Fired, Ack: inc.Ack}
		}
	}
	return attn
}
