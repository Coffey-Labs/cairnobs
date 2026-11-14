package server

import (
	"context"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state"
)

// A notice saved with its incident outlives the copy that saved it: one
// that dies before sending leaves it for the next evaluator (another copy,
// or the same one restarted), which sends it once.
func TestNoticeOutlivesItsCopy(t *testing.T) {
	a, b := twoCopies(t, func(*Config) {})
	phone := &spy{id: "phone", min: "warning"}
	for _, s := range []*Server{a, b} {
		s.alerts.setChannels([]notify.Channel{phone})
	}
	t0 := time.Now()
	a.alerts.evaluate(t0, []Condition{demoDown})
	a.alerts.evaluate(t0.Add(2*time.Minute), []Condition{demoDown}) // fires; a dies before sending
	if due, _ := a.db.DueNotices(time.Now(), 10); len(due) != 1 || due[0].Channel != "phone" {
		t.Fatalf("outbox after firing: %+v", due)
	}
	if len(phone.kinds()) != 0 {
		t.Fatal("sent before anyone ran the outbox")
	}
	b.alerts.load() // b takes over
	b.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown})
	drain(b.alerts)
	drain(b.alerts)
	if want := []string{"firing:critical"}; !eq(phone.kinds(), want) {
		t.Fatalf("phone got %v, want %v", phone.kinds(), want)
	}
	if due, _ := b.db.DueNotices(time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("left in the outbox: %+v", due)
	}
}

// A failed send is tried again after the retry wait, then given up on; a
// notice for a channel that's gone is dropped.
func TestOutboxRetriesAndGivesUp(t *testing.T) {
	a, _ := twoCopies(t, func(*Config) {})
	flaky := &spy{id: "flaky", min: "warning", failFirst: 1}
	dead := &spy{id: "dead", min: "warning", failFirst: 99}
	a.alerts.setChannels([]notify.Channel{flaky, dead})
	a.alerts.retry = []time.Duration{20 * time.Millisecond}
	t0 := time.Now()
	a.alerts.evaluate(t0, []Condition{demoDown})
	a.alerts.evaluate(t0.Add(2*time.Minute), []Condition{demoDown})
	ctx := context.Background()
	a.alerts.sendDue(ctx) // both fail once
	a.alerts.sendDue(ctx) // not due again yet
	if len(flaky.kinds()) != 0 {
		t.Fatal("retried before the wait")
	}
	time.Sleep(30 * time.Millisecond)
	a.alerts.sendDue(ctx) // flaky gets through; dead fails again and is given up
	if want := []string{"firing:critical"}; !eq(flaky.kinds(), want) {
		t.Fatalf("flaky got %v", flaky.kinds())
	}
	if due, _ := a.db.DueNotices(time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("left in the outbox: %+v", due)
	}
	// A channel removed while its notice waited: the notice goes.
	_ = a.db.QueueNotices([]state.OutboxItem{{Made: time.Now(), Channel: "removed", Notice: `{"kind":"firing"}`}}, nil)
	a.alerts.sendDue(ctx)
	if due, _ := a.db.DueNotices(time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("a notice for a removed channel stayed: %+v", due)
	}
}

// With the database away, a notice still goes, from memory.
func TestNoticeWithoutTheDatabase(t *testing.T) {
	a, _ := twoCopies(t, func(*Config) {})
	phone := &spy{id: "phone", min: "warning"}
	a.alerts.setChannels([]notify.Channel{phone})
	t0 := time.Now()
	a.alerts.evaluate(t0, []Condition{demoDown})
	a.db.Close()
	a.alerts.evaluate(t0.Add(2*time.Minute), []Condition{demoDown})
	drain(a.alerts)
	if want := []string{"firing:critical"}; !eq(phone.kinds(), want) {
		t.Fatalf("phone got %v, want %v", phone.kinds(), want)
	}
}
