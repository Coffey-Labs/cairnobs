package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/report"
)

// spy is a channel that records what it was sent; failFirst makes the first
// n sends fail, to exercise retries.
type spy struct {
	id, min   string
	mu        sync.Mutex
	got       []notify.Notice
	failFirst int
}

func (s *spy) ID() string   { return s.id }
func (s *spy) Type() string { return "spy" }
func (s *spy) Min() string  { return s.min }
func (s *spy) Send(_ context.Context, n notify.Notice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFirst > 0 {
		s.failFirst--
		return errors.New("spy: down")
	}
	s.got = append(s.got, n)
	return nil
}
func (s *spy) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []string
	for _, n := range s.got {
		k = append(k, n.Kind+":"+n.Severity)
	}
	return k
}

func testAlerter(chans ...notify.Channel) *alerter {
	a, _ := newAlerter(AlertsConfig{For: 2 * time.Minute, Repeat: 15 * time.Minute, PublicURL: "https://beacon.test"})
	a.channels = chans
	a.retry = []time.Duration{time.Millisecond, time.Millisecond}
	return a
}

// drain delivers everything queued so far: in the outbox, with a
// database, and in memory.
func drain(a *alerter) {
	if a.db != nil {
		for a.sendDue(context.Background()) {
		}
	}
	for {
		select {
		case j := <-a.queue:
			for _, ch := range j.chans {
				a.deliver(context.Background(), ch, j.n)
			}
		default:
			return
		}
	}
}

var demoDown = Condition{Key: "container/node2/inbuxa-demo", Severity: "critical", Title: "inbuxa-demo on node2: restarting", Host: "node2", Href: "/containers"}

func eq(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestBlipNeverPages(t *testing.T) {
	ch := &spy{id: "phone", min: "warning"}
	a := testAlerter(ch)
	t0 := time.Now()
	a.evaluate(t0, []Condition{demoDown})
	a.evaluate(t0.Add(90*time.Second), []Condition{demoDown})
	a.evaluate(t0.Add(105*time.Second), nil) // gone before 2 minutes
	drain(a)
	open, recent := a.snapshot()
	if len(ch.kinds()) != 0 || len(open) != 0 || len(recent) != 0 {
		t.Fatalf("a blip paged: %v open=%d recent=%d", ch.kinds(), len(open), len(recent))
	}
}

func TestFireRepeatAckResolve(t *testing.T) {
	ch := &spy{id: "phone", min: "warning"}
	a := testAlerter(ch)
	t0 := time.Now()
	a.evaluate(t0, []Condition{demoDown})
	a.evaluate(t0.Add(2*time.Minute), []Condition{demoDown}) // fires
	a.evaluate(t0.Add(10*time.Minute), []Condition{demoDown})
	a.evaluate(t0.Add(17*time.Minute), []Condition{demoDown}) // 15 min after firing: repeats
	drain(a)
	if want := []string{"firing:critical", "still:critical"}; !eq(ch.kinds(), want) {
		t.Fatalf("got %v, want %v", ch.kinds(), want)
	}
	open, _ := a.snapshot()
	if open[0].ID != "B-0001" || open[0].Fired == nil {
		t.Fatalf("open: %+v", open)
	}
	if _, ok := a.acknowledge("B-0001", "Pat Admin", t0.Add(18*time.Minute), true); !ok {
		t.Fatal("ack failed")
	}
	a.evaluate(t0.Add(40*time.Minute), []Condition{demoDown}) // acknowledged: no more repeats
	a.evaluate(t0.Add(41*time.Minute), nil)                   // fixed
	drain(a)
	if want := []string{"firing:critical", "still:critical", "resolved:critical"}; !eq(ch.kinds(), want) {
		t.Fatalf("got %v, want %v", ch.kinds(), want)
	}
	_, recent := a.snapshot()
	if len(recent) != 1 || recent[0].Resolved == nil || recent[0].Ack == nil || recent[0].Ack.By != "Pat Admin" {
		t.Fatalf("history: %+v", recent)
	}
	if ch.got[0].URL != "https://beacon.test/containers" {
		t.Fatalf("link %q", ch.got[0].URL)
	}
}

func TestEscalationSendsAgainAndClearsAck(t *testing.T) {
	ch := &spy{id: "ops", min: "warning"}
	a := testAlerter(ch)
	t0 := time.Now()
	disk := Condition{Key: "disk/node3//var/backups", Severity: "warning", Title: "node3 /var/backups is 84% full", Host: "node3"}
	a.evaluate(t0, []Condition{disk})
	a.evaluate(t0.Add(3*time.Minute), []Condition{disk})
	a.acknowledge("B-0001", "Pat Admin", t0.Add(4*time.Minute), true)
	disk.Severity, disk.Title = "critical", "node3 /var/backups is 91% full"
	a.evaluate(t0.Add(5*time.Minute), []Condition{disk})
	drain(a)
	if want := []string{"firing:warning", "escalated:critical"}; !eq(ch.kinds(), want) {
		t.Fatalf("got %v, want %v", ch.kinds(), want)
	}
	if open, _ := a.snapshot(); open[0].Ack != nil {
		t.Fatal("escalation should clear the acknowledgement")
	}
}

func TestChannelMinimums(t *testing.T) {
	phone := &spy{id: "phone", min: "critical"}
	ops := &spy{id: "ops", min: "warning"}
	a := testAlerter(phone, ops)
	t0 := time.Now()
	mem := Condition{Key: "memory/node1", Severity: "warning", Title: "node1 is low on memory", Host: "node1"}
	a.evaluate(t0, []Condition{mem, demoDown})
	a.evaluate(t0.Add(2*time.Minute), []Condition{mem, demoDown})
	drain(a)
	if !eq(phone.kinds(), []string{"firing:critical"}) || len(ops.kinds()) != 2 {
		t.Fatalf("phone %v, ops %v", phone.kinds(), ops.kinds())
	}
}

func TestDeliveryRetries(t *testing.T) {
	ch := &spy{id: "ops", min: "warning", failFirst: 2}
	a := testAlerter(ch)
	t0 := time.Now()
	a.evaluate(t0, []Condition{demoDown})
	a.evaluate(t0.Add(2*time.Minute), []Condition{demoDown})
	drain(a)
	if !eq(ch.kinds(), []string{"firing:critical"}) {
		t.Fatalf("after two failures: %v", ch.kinds())
	}
}

func TestNeverReportedHostAlerts(t *testing.T) {
	srv, _ := testServer(t, false)
	if _, conds := srv.view(srv.started.Add(30 * time.Second)); len(conds) != 0 {
		t.Fatalf("too early: %+v", conds)
	}
	_, conds := srv.view(srv.started.Add(2 * time.Minute))
	if len(conds) != 2 || conds[0].Key != "silent/node1" || conds[0].Severity != "critical" {
		t.Fatalf("never-reported hosts: %+v", conds)
	}
}

func TestAlertAPI(t *testing.T) {
	srv, _ := testServer(t, true)
	ch := &spy{id: "ops", min: "warning"}
	srv.alerts.channels = []notify.Channel{ch}
	t0 := time.Now()
	srv.alerts.evaluate(t0, []Condition{demoDown})
	srv.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown})
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]

	rec := get(h, "/api/v1/incidents", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"B-0001"`) {
		t.Fatalf("incidents: %d %s", rec.Code, rec.Body)
	}
	// Acknowledging needs the X-Beacon header, like every change.
	req := httptest.NewRequest("POST", "/api/v1/incidents/B-0001/ack", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ack without header: %d", rec.Code)
	}
	req.Header.Set("X-Beacon", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"by":"Developer (local)"`) {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/overview", cookie); !strings.Contains(rec.Body.String(), `"alerts":{"firing":1,"unacknowledged":0}`) {
		t.Fatalf("overview alerts: %s", rec.Body)
	}
	req = httptest.NewRequest("POST", "/api/v1/alerts/test", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Beacon", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"result":"sent"`) || !eq(ch.kinds(), []string{"test:warning"}) {
		t.Fatalf("test alert: %s %v", rec.Body, ch.kinds())
	}
}

func TestMaintenanceHoldsAHostsAlerts(t *testing.T) {
	ch := &spy{id: "phone", min: "warning"}
	a := testAlerter(ch)
	t0 := time.Now()
	disk := Condition{Key: "disk/node2//", Severity: "warning", Title: "node2 / is 85% full", Host: "node2"}
	a.evaluate(t0, []Condition{disk})
	a.evaluate(t0.Add(3*time.Minute), []Condition{disk}) // fires
	held := map[string]bool{"node2": true}
	silent := Condition{Key: "silent/node2", Severity: "critical", Title: "node2 has gone silent", Host: "node2"}
	gitDown := Condition{Key: "container/node1/gitea", Severity: "critical", Title: "gitea on node1: exited", Host: "node1"} // another host: not held
	// node2 reboots: its disk reading disappears and it goes silent, for ten minutes.
	for m := 4; m <= 14; m++ {
		a.evaluateHeld(t0.Add(time.Duration(m)*time.Minute), []Condition{silent, gitDown}, held)
	}
	drain(a)
	open, _ := a.snapshot()
	if want := []string{"firing:warning", "firing:critical"}; !eq(ch.kinds(), want) || len(open) != 2 {
		t.Fatalf("during the window: %v, %d open", ch.kinds(), len(open))
	}
	// Window over, node2 is back and the disk is fine: now it resolves; silence never paged.
	a.evaluate(t0.Add(15*time.Minute), []Condition{gitDown})
	drain(a)
	if want := []string{"firing:warning", "firing:critical", "resolved:warning"}; !eq(ch.kinds(), want) {
		t.Fatalf("after: %v", ch.kinds())
	}
}

func TestMaintenanceConfig(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/obs-beacond.yaml"
	write := func(s string) { _ = os.WriteFile(p, []byte(s), 0o600) }
	agents := "agents:\n  - name: node1\n    token_sha256: \"" + HashToken("x") + "\"\n"
	write(agents + "maintenance:\n  - name: reboot\n    hosts: [node1]\n    daily: \"04:30\"\n    timezone: Europe/Berlin\n    duration: 20m\n")
	cfg, err := LoadConfig(p)
	if err != nil || len(cfg.Maintenance) != 1 {
		t.Fatalf("good window: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if held := srv.heldHosts(time.Date(2026, 10, 16, 4, 40, 0, 0, berlin)); !held["node1"] {
		t.Fatal("node1 not held in its window")
	}
	write(agents + "maintenance:\n  - name: reboot\n    hosts: [node9]\n    daily: \"04:30\"\n    duration: 20m\n")
	// The name is checked when obs-beacond starts, against joined agents too.
	cfg, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), `no agent called "node9"`) {
		t.Fatalf("unknown host accepted: %v", err)
	}
}

func TestCriticalNeverEmailOnly(t *testing.T) {
	pw := t.TempDir() + "/pw"
	_ = os.WriteFile(pw, []byte("x"), 0o600)
	mail := notify.Config{ID: "mail", Type: "email", Min: "serious", SMTP: "mail.test:587", PasswordFile: pw, From: "a@b.test", To: []string{"c@d.test"}}
	if _, err := newAlerter(AlertsConfig{Channels: []notify.Config{mail}}); err == nil || !strings.Contains(err.Error(), "only go out by email") {
		t.Fatalf("email-only accepted: %v", err)
	}
	if _, err := newAlerter(AlertsConfig{Channels: []notify.Config{mail, {ID: "dev", Type: "log", Min: "critical"}}}); err != nil {
		t.Fatalf("email plus another: %v", err)
	}
}

func TestIncidentCarriesContainerLogs(t *testing.T) {
	srv, _ := testServer(t, true)
	now := time.Now()
	rep := report.Report{V: report.Version, Containers: &report.Containers{List: []report.Container{
		{ID: "c1", Name: "gitea", State: "restarting", Status: "Restarting (1) 5 seconds ago",
			Logs: []string{"2026-10-15T21:00:00Z panic: can't reach the database"}},
	}}}
	srv.store.Put("node1", rep, now)
	_, conds := srv.view(now)
	srv.alerts.evaluate(now, conds)
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	body := get(h, "/api/v1/incidents", cookie).Body.String()
	if !strings.Contains(body, `"key":"container/node1/gitea"`) || !strings.Contains(body, `"logs":["2026-10-15T21:00:00Z panic: can't reach the database"]`) {
		t.Fatalf("incidents: %s", body)
	}
}

// follower is a spy that follows incidents, as PagerDuty does.
type follower struct{ spy }

func (f *follower) Follows() bool { return true }

func TestAcknowledgingTellsFollowersOnly(t *testing.T) {
	phone, pd := &spy{id: "phone", min: "warning"}, &follower{spy{id: "pd", min: "warning"}}
	a := testAlerter(phone, pd)
	t0 := time.Now()
	a.evaluate(t0, []Condition{demoDown})
	a.evaluate(t0.Add(2*time.Minute), []Condition{demoDown}) // fires
	a.acknowledge("B-0001", "Pat Admin", t0.Add(3*time.Minute), true)
	a.evaluate(t0.Add(4*time.Minute), nil)
	drain(a)
	if want := []string{"firing:critical", "resolved:critical"}; !eq(phone.kinds(), want) {
		t.Fatalf("phone got %v", phone.kinds())
	}
	if want := []string{"firing:critical", "acknowledged:critical", "resolved:critical"}; !eq(pd.kinds(), want) {
		t.Fatalf("follower got %v", pd.kinds())
	}
	// Acknowledging one nobody was told of tells nobody.
	b := testAlerter(pd)
	b.evaluate(t0, []Condition{demoDown})
	b.acknowledge("B-0001", "Pat Admin", t0.Add(time.Minute), true)
	if len(b.queue) != 0 {
		t.Fatal("acknowledging a held incident told the follower")
	}
}
