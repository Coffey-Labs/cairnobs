package server

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func pgOnly(t *testing.T) {
	t.Helper()
	if !statetest.Postgres() {
		t.Skipf("PostgreSQL only: set %s", statetest.Env)
	}
}

// Alone, a copy is always the evaluator.
func TestAloneIsTheEvaluator(t *testing.T) {
	statetest.SQLiteOnly(t)
	a, _ := twoCopies(t, func(*Config) {})
	if !a.leading() {
		t.Fatal("a lone copy isn't the evaluator")
	}
}

// One copy holds the lease and runs the evaluator's jobs. When it stops
// renewing (paused, say), another takes over from the incidents as stored:
// nothing fires twice, and the paused copy, waking, tells nobody.
func TestOneEvaluatorAtATime(t *testing.T) {
	pgOnly(t)
	a, b := twoCopies(t, func(*Config) {})
	phone := &spy{id: "phone", min: "warning"}
	for _, s := range []*Server{a, b} {
		s.alerts.setChannels(testAlerter(phone).channels)
		s.alerts.retry = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var aJobs, bJobs atomic.Int32
	a.tryLead(ctx, func(context.Context) { aJobs.Add(1) })
	b.tryLead(ctx, func(context.Context) { bJobs.Add(1) })
	eventually(t, "a's jobs start", func() bool { return aJobs.Load() == 1 })
	if !a.leading() || b.leading() || bJobs.Load() != 0 {
		t.Fatalf("leading: a %v b %v, b's jobs %d", a.leading(), b.leading(), bJobs.Load())
	}
	// Renewing keeps the epoch and doesn't start the jobs again.
	a.tryLead(ctx, func(context.Context) { aJobs.Add(1) })
	if a.lead.fence.Epoch != 1 || aJobs.Load() != 1 {
		t.Fatalf("renewal: epoch %d, jobs %d", a.lead.fence.Epoch, aJobs.Load())
	}

	t0 := time.Now()
	a.alerts.evaluate(t0, []Condition{demoDown})
	a.alerts.evaluate(t0.Add(2*time.Minute), []Condition{demoDown}) // fires
	drain(a.alerts)

	// a stalls: its lease runs out without it noticing.
	db, err := sql.Open("pgx", a.cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE leases SET until_ms = 0`); err != nil {
		t.Fatal(err)
	}
	b.tryLead(ctx, func(context.Context) { bJobs.Add(1) })
	eventually(t, "b's jobs start", func() bool { return bJobs.Load() == 1 })
	if !b.leading() || b.lead.fence.Epoch != 2 {
		t.Fatalf("b didn't take over: %+v", b.lead.fence)
	}
	b.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown}) // still down: already fired
	drain(b.alerts)

	// a wakes, still thinking it leads, and sees the problem gone. Its
	// save is refused, so it says nothing and steps down.
	a.alerts.onFenced = func() { a.stepDown("test: its save was refused") }
	a.alerts.evaluate(t0.Add(4*time.Minute), nil)
	drain(a.alerts)
	if a.leading() {
		t.Fatal("a still thinks it leads")
	}
	if want := []string{"firing:critical"}; !eq(phone.kinds(), want) {
		t.Fatalf("after a woke: %v", phone.kinds())
	}
	if open, _, _ := b.db.Incidents(10); len(open) != 1 {
		t.Fatalf("a's refused save resolved it anyway: %+v", open)
	}

	// b sees it resolve, and numbers the next one after a's.
	b.alerts.evaluate(t0.Add(5*time.Minute), nil)
	b.alerts.evaluate(t0.Add(6*time.Minute), []Condition{{Key: "host/node3/down", Severity: "critical", Title: "node3 down", Host: "node3"}})
	drain(b.alerts)
	if want := []string{"firing:critical", "resolved:critical"}; !eq(phone.kinds(), want) {
		t.Fatalf("phone got %v", phone.kinds())
	}
	if open, _ := b.alerts.snapshot(); len(open) != 1 || open[0].ID != "B-0002" {
		t.Fatalf("b's next incident: %+v", open)
	}

	// On the way out the lease is given up and announced, and a takes it
	// at once.
	go a.follow(ctx)
	<-a.caughtUp
	b.stepDown("")
	eventually(t, "a hears of the release", func() bool { return len(a.lead.poke) == 1 })
	a.tryLead(ctx, func(context.Context) { aJobs.Add(1) })
	if !a.leading() || a.lead.fence.Epoch != 3 {
		t.Fatalf("a didn't take the released lease: %+v", a.lead.fence)
	}
}

// An acknowledgement taken on a copy that isn't the evaluator is stored at
// once, and the evaluator tells the channels that follow incidents.
func TestAckOnAnotherCopy(t *testing.T) {
	pgOnly(t)
	a, b := twoCopies(t, func(c *Config) { c.DevSignin = true })
	pd := &follower{spy{id: "pd", min: "warning"}}
	for _, s := range []*Server{a, b} {
		s.alerts.setChannels(testAlerter(pd).channels)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.tryLead(ctx, func(context.Context) {})
	for _, s := range []*Server{a, b} {
		go s.follow(ctx)
		<-s.caughtUp
	}
	t0 := time.Now()
	a.alerts.evaluate(t0, []Condition{demoDown})
	if a.alerts.evaluateHeld(t0.Add(2*time.Minute), []Condition{demoDown}, nil) {
		a.publish(noticeIncidents, "")
	}
	eventually(t, "b has the incident", func() bool { open, _ := b.alerts.snapshot(); return len(open) == 1 && open[0].Fired != nil })

	c := devCookie(t, b.Handler())
	if rec := sendCookie(b.Handler(), "POST", "/api/v1/incidents/B-0001/ack", c, ""); rec.Code != 200 {
		t.Fatalf("ack on b: %d %s", rec.Code, rec.Body)
	}
	eventually(t, "a has the ack", func() bool { open, _ := a.alerts.snapshot(); return len(open) == 1 && open[0].Ack != nil })
	drain(a.alerts)
	drain(b.alerts)
	if want := []string{"firing:critical", "acknowledged:critical"}; !eq(pd.kinds(), want) {
		t.Fatalf("follower got %v", pd.kinds())
	}
	// The evaluator's next save keeps it.
	a.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown})
	if si, _ := a.db.Incident("B-0001"); si.AckBy == "" {
		t.Fatal("the evaluator's save lost the acknowledgement")
	}
}
