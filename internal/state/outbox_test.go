package state

import (
	"testing"
	"time"
)

// A save the lease no longer allows queues nothing; one it does queues its
// notices with it.
func TestOutboxGoesWithTheSave(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	f, ok, err := db.Acquire("evaluator", "me", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	inc := Incident{ID: "B-0001", Key: "k", Severity: "critical", Title: "t", Opened: now, Fired: now, Notified: now}
	n := []OutboxItem{{Made: now, Channel: "phone", Notice: `{"kind":"firing"}`}, {Made: now, Channel: "mail", Notice: `{"kind":"firing"}`}}
	if err := db.PutIncidentNotices(inc, &f, n); err != nil {
		t.Fatal(err)
	}
	stale := f
	stale.Epoch++
	if err := db.PutIncidentNotices(inc, &stale, n); err != ErrFenced {
		t.Fatalf("stale fence: %v", err)
	}
	if err := db.QueueNotices(n, &stale); err != ErrFenced {
		t.Fatalf("stale fence, queue alone: %v", err)
	}
	due, err := db.DueNotices(now, 10)
	if err != nil || len(due) != 2 || due[0].Attempts != 0 {
		t.Fatalf("due: %+v %v", due, err)
	}
	if err := db.NoticeLater(due[0].ID, 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = db.NoticeDone(due[1].ID)
	if d, _ := db.DueNotices(now, 10); len(d) != 0 {
		t.Fatalf("due now: %+v", d)
	}
	if d, _ := db.DueNotices(now.Add(2*time.Minute), 10); len(d) != 1 || d[0].Attempts != 1 {
		t.Fatalf("due later: %+v", d)
	}
}
