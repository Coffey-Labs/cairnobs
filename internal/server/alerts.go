package server

// The alert engine. Every evaluation, the conditions from view() are matched
// to incidents by key:
//
//   - a new condition opens an incident, but nobody is told until it has
//     lasted alerts.for (2 minutes), so a blip doesn't page anyone;
//   - once it fires, every channel whose minimum it reaches is told;
//   - a critical incident nobody has acknowledged is repeated every
//     alerts.repeat (15 minutes); one that gets worse is sent again at once;
//   - when the condition goes away the incident resolves, and the channels
//     that heard about it hear that too. One that never fired just goes.
//
// Notices are sent apart from evaluation, with retries, so a slow channel
// never holds it up. With a database, incidents are kept there, so a
// restart (or another copy taking over as evaluator, lead.go) carries on
// with them, and so are the notices: each goes into the outbox in the same
// transaction as its incident's save, and the evaluator sends from there
// (outbox.go). An evaluator whose lease has passed (the save is refused)
// queues nothing; a notice it did queue is sent by whoever takes over.
// Without a database, notices go through a queue in memory.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state"
)

// AlertsConfig is the alerts section of obs-beacond.yaml.
type AlertsConfig struct {
	For       time.Duration   `yaml:"for"`        // how long a problem lasts before anyone is told (2m)
	Repeat    time.Duration   `yaml:"repeat"`     // how often an unacknowledged critical is repeated (15m)
	PublicURL string          `yaml:"public_url"` // Cairn OBS's address, for links in notices
	Channels  []notify.Config `yaml:"channels"`
}

// Incident is one problem, from first seen to resolved.
type Incident struct {
	ID       string      `json:"id"`
	Key      string      `json:"key"`
	Severity string      `json:"severity"`
	Title    string      `json:"title"`
	Detail   string      `json:"detail"`
	Host     string      `json:"host"`
	Href     string      `json:"href"`
	Opened   time.Time   `json:"opened"`
	Fired    *time.Time  `json:"fired"`    // when people were first told; nil while holding back
	Notified *time.Time  `json:"notified"` // the last notice
	Resolved *time.Time  `json:"resolved"`
	Ack      *Ack        `json:"ack"`
	Affected string      `json:"affected,omitempty"` // what else it takes down, from root-cause grouping
	Grouped  []GroupedBy `json:"grouped,omitempty"`  // the learned dependencies that grouped it
	Map      string      `json:"map,omitempty"`      // where it is on the maps
}

// same says whether nothing worth saving changed.
func (i Incident) same(o Incident) bool {
	g1, g2 := i.Grouped, o.Grouped
	i.Grouped, o.Grouped = nil, nil
	return reflect.DeepEqual(i, o) && slices.Equal(g1, g2)
}

// grouping is what's kept of an incident's root-cause grouping.
type grouping struct {
	Affected string      `json:"affected,omitempty"`
	Grouped  []GroupedBy `json:"grouped,omitempty"`
	Map      string      `json:"map,omitempty"`
}

// Ack records who acknowledged an incident.
type Ack struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type job struct {
	n     notify.Notice
	chans []notify.Channel
	inc   string // the incident it's about; saved with it into the outbox
}

type alerter struct {
	cfg AlertsConfig

	chMu     sync.RWMutex
	channels []notify.Channel // replaced whole when the UI changes them; read with chans()
	retry    []time.Duration  // waits between attempts

	db  *state.DB      // nil: incidents live in memory only
	loc *time.Location // notices give times here: the reports' timezone (nil: UTC)

	mu       sync.Mutex
	open     map[string]*Incident // by condition key
	history  []Incident           // resolved, newest first
	seq      int
	queue    chan job
	pend     []job         // notices waiting for their incident's save (under mu)
	fence    *state.Fence  // the evaluator lease, when copies share the database (under mu)
	fenced   bool          // a save was refused in this round (under mu)
	onFenced func()        // told when a save is refused: this copy lost the lease
	changed  bool          // something was saved in this round (under mu)
	wake     chan struct{} // the outbox has something new
}

const historyKeep = 200

func newAlerter(cfg AlertsConfig) (*alerter, error) {
	if cfg.For == 0 {
		cfg.For = 2 * time.Minute
	}
	if cfg.Repeat == 0 {
		cfg.Repeat = 15 * time.Minute
	}
	a := &alerter{cfg: cfg, open: map[string]*Incident{}, queue: make(chan job, 256), wake: make(chan struct{}, 1), retry: []time.Duration{5 * time.Second, 30 * time.Second}}
	chans, err := buildChannels(cfg.Channels)
	if err != nil {
		return nil, err
	}
	a.channels = chans
	return a, nil
}

// buildChannels makes the channels from their settings, reading their
// secrets now, and checks them as a set.
func buildChannels(cfgs []notify.Config) ([]notify.Channel, error) {
	var out []notify.Channel
	ids := map[string]bool{}
	for _, c := range cfgs {
		ch, err := notify.New(c)
		if err != nil {
			return nil, err
		}
		if ids[ch.ID()] {
			return nil, fmt.Errorf("two alert channels called %q", ch.ID())
		}
		ids[ch.ID()] = true
		out = append(out, ch)
	}
	// If mail is what's down, an email about it never arrives: critical
	// alerts must also reach at least one channel that isn't email.
	mailOnly := false
	for _, ch := range out {
		if notify.Rank(ch.Min()) <= notify.Rank("critical") {
			if ch.Type() != "email" {
				return out, nil
			}
			mailOnly = true
		}
	}
	if mailOnly {
		return nil, fmt.Errorf("critical alerts would only go out by email; add a channel that isn't email (ntfy, Discord, webhook, PagerDuty) taking critical")
	}
	return out, nil
}

// chans are the channels in force. The slice is never changed in place.
func (a *alerter) chans() []notify.Channel {
	a.chMu.RLock()
	defer a.chMu.RUnlock()
	return a.channels
}

func (a *alerter) setChannels(chans []notify.Channel) {
	a.chMu.Lock()
	a.channels = chans
	a.chMu.Unlock()
}

func (a *alerter) zone() *time.Location {
	if a.loc == nil {
		return time.UTC
	}
	return a.loc
}

func (a *alerter) notice(inc *Incident, kind string) notify.Notice {
	n := notify.Notice{Kind: kind, ID: inc.ID, Severity: inc.Severity, Title: inc.Title, Detail: inc.Detail, Host: inc.Host, Since: inc.Opened.In(a.zone())}
	if a.cfg.PublicURL != "" && inc.Href != "" {
		n.URL = a.cfg.PublicURL + inc.Href
	}
	return n
}

// enqueue sends to every channel whose minimum the severity reaches.
// Called with a.mu held.
func (a *alerter) enqueue(inc *Incident, kind string, now time.Time) {
	t := now
	inc.Notified = &t
	var to []notify.Channel
	for _, ch := range a.chans() {
		if notify.Rank(inc.Severity) >= notify.Rank(ch.Min()) {
			to = append(to, ch)
		}
	}
	if len(to) == 0 {
		return
	}
	a.pend = append(a.pend, job{n: a.notice(inc, kind), chans: to, inc: inc.ID})
}

// tellFollowers tells the channels that follow incidents (PagerDuty) about
// one they were told of; the others hear only of new and resolved
// problems. Called with a.mu held.
func (a *alerter) tellFollowers(inc *Incident, kind string) {
	if inc.Fired == nil {
		return // nobody was told of it
	}
	var to []notify.Channel
	for _, ch := range a.chans() {
		if f, ok := ch.(notify.Follower); ok && f.Follows() && notify.Rank(inc.Severity) >= notify.Rank(ch.Min()) {
			to = append(to, ch)
		}
	}
	if len(to) == 0 {
		return
	}
	a.pend = append(a.pend, job{n: a.notice(inc, kind), chans: to})
}

// flush queues the notices of a round not already saved with their
// incident, unless a save in it was refused; the caller holds a.mu.
func (a *alerter) flush() {
	pend := a.pend
	a.pend = nil
	if a.fenced {
		if len(pend) > 0 {
			log.Printf("alerts: not sending %d notices: another copy is the evaluator now", len(pend))
		}
		return
	}
	if a.db != nil {
		if len(pend) == 0 {
			return
		}
		err := a.db.QueueNotices(outboxItems(pend, time.Now()), a.fence)
		a.checkFence(err)
		switch {
		case err == nil:
			a.poke()
			return
		case err == state.ErrFenced:
			return
		}
		// The database is away: send them from memory rather than not at all.
		log.Printf("alerts: can't queue %d notices in the database, sending them from memory: %v", len(pend), err)
	}
	for _, j := range pend {
		select {
		case a.queue <- j:
		default:
			log.Printf("alerts: queue full, dropped %s %s", j.n.Kind, j.n.ID)
		}
	}
}

// tellEveryone sends a notice to every channel, whatever its minimum: for
// things everyone must hear about, like the emergency door being used.
func (a *alerter) tellEveryone(n notify.Notice) {
	chans := a.chans()
	if len(chans) == 0 {
		return
	}
	n.URL = a.cfg.PublicURL
	select {
	case a.queue <- job{n: n, chans: chans}:
	default:
		log.Printf("alerts: queue full, dropped %q", n.Title)
	}
}

func t0(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func tp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// save writes an incident through to the database. Called with a.mu held.
func (a *alerter) save(inc *Incident) {
	if a.db == nil {
		return
	}
	si := state.Incident{ID: inc.ID, Key: inc.Key, Severity: inc.Severity, Title: inc.Title, Detail: inc.Detail, Host: inc.Host, Href: inc.Href,
		Opened: inc.Opened, Fired: t0(inc.Fired), Notified: t0(inc.Notified), Resolved: t0(inc.Resolved)}
	if inc.Ack != nil {
		si.AckBy, si.AckAt = inc.Ack.By, inc.Ack.At
	}
	if inc.Affected != "" || len(inc.Grouped) > 0 || inc.Map != "" {
		b, _ := json.Marshal(grouping{inc.Affected, inc.Grouped, inc.Map})
		si.Grouping = string(b)
	}
	a.changed = true
	// The notices about it go into the outbox with it.
	var mine, rest []job
	for _, j := range a.pend {
		if j.inc == inc.ID {
			mine = append(mine, j)
		} else {
			rest = append(rest, j)
		}
	}
	err := a.db.PutIncidentNotices(si, a.fence, outboxItems(mine, time.Now()))
	a.checkFence(err)
	if err != nil {
		if err != state.ErrFenced {
			log.Printf("alerts: can't store %s: %v", inc.ID, err)
		}
		return // its notices stay pending: flush drops them if fenced, else queues them
	}
	a.pend = rest
	if len(mine) > 0 {
		a.poke()
	}
}

// checkFence notes a refused save: this copy isn't the evaluator any more.
// The caller holds a.mu.
func (a *alerter) checkFence(err error) {
	if err != state.ErrFenced || a.fenced {
		return
	}
	a.fenced = true
	a.fence = nil
	log.Printf("alerts: another copy has taken over as evaluator")
	if a.onFenced != nil {
		a.onFenced()
	}
}

// lead makes this copy the evaluator under f (nil: it isn't one), starting
// from the incidents as stored.
func (a *alerter) lead(f *state.Fence) {
	if f != nil {
		a.load()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fence, a.fenced, a.pend = f, false, nil
}

func fromState(si state.Incident) *Incident {
	inc := &Incident{ID: si.ID, Key: si.Key, Severity: si.Severity, Title: si.Title, Detail: si.Detail, Host: si.Host, Href: si.Href,
		Opened: si.Opened, Fired: tp(si.Fired), Notified: tp(si.Notified), Resolved: tp(si.Resolved)}
	if si.AckBy != "" {
		inc.Ack = &Ack{By: si.AckBy, At: si.AckAt}
	}
	var g grouping
	if si.Grouping != "" && json.Unmarshal([]byte(si.Grouping), &g) == nil {
		inc.Affected, inc.Grouped, inc.Map = g.Affected, g.Grouped, g.Map
	}
	return inc
}

// load reads the incidents as stored, replacing what's in memory: at
// start, on taking over as evaluator, and on copies that aren't the
// evaluator whenever it says they changed. After a restart that means an
// upgrade neither forgets an alert nor sends it again.
func (a *alerter) load() {
	if a.db == nil {
		return
	}
	open, recent, err := a.db.Incidents(historyKeep)
	if err != nil {
		log.Printf("alerts: can't load incidents: %v", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	first := len(a.open) == 0 && len(a.history) == 0
	a.open, a.history = map[string]*Incident{}, nil
	for _, si := range open {
		a.open[si.Key] = fromState(si)
	}
	for _, si := range recent {
		a.history = append(a.history, *fromState(si))
	}
	fmt.Sscan(a.db.Meta("incident_seq"), &a.seq)
	if first && len(open) > 0 {
		log.Printf("alerts: %d open incidents brought back from the database", len(open))
	}
}

// evaluate matches this round's conditions to incidents.
func (a *alerter) evaluate(now time.Time, conds []Condition) { a.evaluateHeld(now, conds, nil) }

// evaluateHeld is evaluate with some hosts in a maintenance window: their
// problems don't open or fire anything, and incidents already open for them
// stay exactly as they are (no repeats, and no "resolved" while the host is
// simply rebooting). When the window closes they're judged normally again.
func (a *alerter) evaluateHeld(now time.Time, conds []Condition, held map[string]bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.changed = false
	defer a.flush()
	seen := map[string]bool{}
	for _, c := range conds {
		if c.Host != "" && held[c.Host] {
			continue
		}
		seen[c.Key] = true
		inc := a.open[c.Key]
		if inc == nil {
			a.seq++
			if a.db != nil {
				a.checkFence(a.db.SetMetaAs("incident_seq", fmt.Sprint(a.seq), a.fence))
			}
			inc = &Incident{ID: fmt.Sprintf("B-%04d", a.seq), Key: c.Key, Opened: now}
			a.open[c.Key] = inc
		}
		before := *inc
		worse := inc.Fired != nil && notify.Rank(c.Severity) > notify.Rank(inc.Severity)
		inc.Severity, inc.Title, inc.Detail, inc.Host, inc.Href, inc.Affected = c.Severity, c.Title, c.Detail, c.Host, c.Href, c.Affected
		inc.Grouped, inc.Map = c.Grouped, c.Map
		switch {
		case inc.Fired == nil && now.Sub(inc.Opened) >= a.cfg.For:
			t := now
			inc.Fired = &t
			a.enqueue(inc, "firing", now)
		case worse:
			if inc.Ack != nil && a.db != nil && !a.fenced {
				_ = a.db.ClearAck(inc.ID)
			}
			inc.Ack = nil // it changed; someone needs to look again
			a.enqueue(inc, "escalated", now)
		case inc.Fired != nil && inc.Severity == "critical" && inc.Ack == nil && now.Sub(*inc.Notified) >= a.cfg.Repeat:
			a.enqueue(inc, "still", now)
		}
		if !inc.same(before) || before.ID == "" || inc.Opened.Equal(now) {
			a.save(inc)
		}
	}
	for key, inc := range a.open {
		if seen[key] || (inc.Host != "" && held[inc.Host]) {
			continue
		}
		delete(a.open, key)
		if inc.Fired == nil {
			if a.db != nil && !a.fenced { // a blip shorter than alerts.for: nobody was told, nothing to keep
				_ = a.db.DeleteIncident(inc.ID)
				a.changed = true
			}
			continue
		}
		t := now
		inc.Resolved = &t
		a.enqueue(inc, "resolved", now)
		a.save(inc)
		a.history = append([]Incident{*inc}, a.history...)
		if len(a.history) > historyKeep {
			a.history = a.history[:historyKeep]
		}
	}
	return a.changed
}

// acknowledge stops the repeats for an open incident. With a database the
// acknowledgement is written there on its own, so any copy may take it;
// tell says whether this copy also tells the channels that follow
// incidents (it's the evaluator, or the only copy).
func (a *alerter) acknowledge(id, by string, now time.Time, tell bool) (Incident, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, inc := range a.open {
		if inc.ID == id {
			if a.db != nil {
				if ok, err := a.db.AckIncident(id, by, now); err != nil || !ok {
					return Incident{}, false
				}
			}
			inc.Ack = &Ack{By: by, At: now}
			if tell {
				a.tellFollowers(inc, "acknowledged")
				a.flush()
			}
			return *inc, true
		}
	}
	return Incident{}, false
}

// acknowledged takes in an acknowledgement another copy made, as stored,
// and, when tell, tells the channels that follow incidents.
func (a *alerter) acknowledged(id string, tell bool) {
	si, err := a.db.Incident(id)
	if err != nil || si.AckBy == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, inc := range a.open {
		if inc.ID == id && inc.Ack == nil {
			inc.Ack = &Ack{By: si.AckBy, At: si.AckAt}
			if tell {
				a.tellFollowers(inc, "acknowledged")
				a.flush()
			}
		}
	}
}

// snapshot lists open incidents (critical first, then oldest) and recent history.
// firing says whether the condition with this key already has an incident
// people were told about.
func (a *alerter) firing(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	inc := a.open[key]
	return inc != nil && inc.Fired != nil
}

func (a *alerter) snapshot() (open, recent []Incident) {
	a.mu.Lock()
	defer a.mu.Unlock()
	open = []Incident{}
	for _, inc := range a.open {
		open = append(open, *inc)
	}
	sort.Slice(open, func(i, j int) bool {
		if ri, rj := notify.Rank(open[i].Severity), notify.Rank(open[j].Severity); ri != rj {
			return ri > rj
		}
		if !open[i].Opened.Equal(open[j].Opened) {
			return open[i].Opened.Before(open[j].Opened)
		}
		// Many open in one evaluation (a restart, a wide outage), so ties
		// are common: break them on the id, or the list reshuffles with
		// map order on every refresh and a click lands on another row.
		return idLess(open[i].ID, open[j].ID)
	})
	n := min(len(a.history), 50)
	recent = append([]Incident{}, a.history[:n]...)
	return open, recent
}

// idLess orders incident ids by number: B-0999 before B-1000 before B-10000.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// run delivers queued notices until ctx ends.
func (a *alerter) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-a.queue:
			for _, ch := range j.chans {
				a.deliver(ctx, ch, j.n)
			}
		}
	}
}

func (a *alerter) deliver(ctx context.Context, ch notify.Channel, n notify.Notice) {
	for attempt := 0; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := ch.Send(sctx, n)
		cancel()
		if err == nil {
			log.Printf("alerts: sent %s %s (%s) to %s", n.Kind, n.ID, n.Severity, ch.ID())
			return
		}
		if attempt >= len(a.retry) {
			log.Printf("alerts: gave up on %s for %s %s: %v", ch.ID(), n.Kind, n.ID, err)
			return
		}
		log.Printf("alerts: %v; trying again in %s", err, a.retry[attempt])
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.retry[attempt]):
		}
	}
}

// test sends a test notice right away to every channel, or to the one
// called only, and reports each result.
func (a *alerter) test(ctx context.Context, only string) []map[string]string {
	out := []map[string]string{}
	for _, ch := range a.chans() {
		if only != "" && ch.ID() != only {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := ch.Send(sctx, notify.Notice{Kind: "test", Severity: "warning", Title: "Test", Since: time.Now().In(a.zone()), URL: a.cfg.PublicURL})
		cancel()
		res := map[string]string{"id": ch.ID(), "type": ch.Type(), "result": "sent"}
		if err != nil {
			res["result"] = err.Error()
		}
		log.Printf("alerts: test to %s: %s", ch.ID(), res["result"])
		out = append(out, res)
	}
	return out
}

// --- HTTP ---

// evaluate runs one round of the alerts and says whether any incident
// changed.
func (srv *Server) evaluate(now time.Time) bool {
	// Right after a start nothing has reported yet, and incidents brought
	// back from the database would look fixed: wait until every agent has
	// had time to report before judging anything.
	if now.Sub(srv.started) < srv.cfg.StaleAfter {
		return false
	}
	_, conds := srv.view(now)
	conds = srv.groupConditions(conds, srv.alerts.firing)
	return srv.alerts.evaluateHeld(now, conds, srv.heldHosts(now))
}

// IncidentView is an open incident as the Alerts page shows it: with the
// last log lines when it's about a container that has them. Logs stay in
// Cairn OBS: notices carry the link, not the lines.
type IncidentView struct {
	Incident
	Logs []string `json:"logs,omitempty"`
}

func (srv *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	snapOpen, recent := srv.alerts.snapshot()
	hosts, _ := srv.view(time.Now())
	logs := map[string][]string{}
	for _, h := range hosts {
		if !srv.seesAll(r) {
			break // log lines are for admins and operators
		}
		for _, row := range h.rows {
			if len(row.Logs) > 0 {
				logs["container/"+h.Name+"/"+row.Name] = row.Logs
			}
		}
	}
	open := make([]IncidentView, 0, len(snapOpen))
	for _, inc := range snapOpen {
		open = append(open, IncidentView{Incident: inc, Logs: logs[inc.Key]})
	}
	chans := []map[string]string{}
	for _, ch := range srv.alerts.chans() {
		chans = append(chans, map[string]string{"id": ch.ID(), "type": ch.Type(), "min": ch.Min()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"open": open, "recent": recent, "channels": chans,
		"for_s": int(srv.alerts.cfg.For / time.Second), "repeat_s": int(srv.alerts.cfg.Repeat / time.Second), "stored": srv.db != nil,
		"maintenance": srv.maintenanceView(time.Now()), "maintenance_source": srv.maintenanceSource(), "channels_source": srv.channelsSource(),
	})
}

func (srv *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	_, s, _ := srv.sessions.get(r)
	lead := srv.leading()
	inc, ok := srv.alerts.acknowledge(r.PathValue("id"), s.User, time.Now(), lead)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no open incident with that id"})
		return
	}
	if !lead {
		srv.publish(noticeAck, inc.ID) // the evaluator tells the channels that follow incidents
	}
	srv.audit(s.User, "acknowledged", inc.ID+": "+inc.Title)
	writeJSON(w, http.StatusOK, inc)
}

func (srv *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	_, s, _ := srv.sessions.get(r)
	only := r.URL.Query().Get("channel")
	srv.audit(s.User, "sent a test alert", only)
	writeJSON(w, http.StatusOK, map[string]any{"channels": srv.alerts.test(r.Context(), only)})
}

// heldHosts are the hosts in a maintenance window right now.
func (srv *Server) heldHosts(now time.Time) map[string]bool {
	held := map[string]bool{}
	ws := srv.windows()
	for i := range ws {
		w := &ws[i]
		if w.Active(now) {
			for _, h := range w.Hosts {
				held[h] = true
			}
		}
	}
	return held
}

// maintenanceView is what the Alerts page shows about the windows.
func (srv *Server) maintenanceView(now time.Time) []map[string]any {
	out := []map[string]any{}
	ws := srv.windows()
	for i := range ws {
		w := &ws[i]
		out = append(out, map[string]any{"name": w.Name, "hosts": w.Hosts, "daily": w.Daily, "timezone": w.Timezone,
			"duration_s": int(w.Duration / time.Second), "active": w.Active(now), "next": w.Next(now)})
	}
	return out
}
