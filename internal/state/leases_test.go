package state

import (
	"testing"
	"time"
)

func TestLeaseTakeRenewAndFence(t *testing.T) {
	db, _ := open(t)
	t0 := time.UnixMilli(1_800_000_000_000)
	a, ok, err := db.Acquire("evaluator", "copy-a", 15*time.Second, t0)
	if err != nil || !ok || a.Epoch != 1 {
		t.Fatalf("a: %+v %v %v", a, ok, err)
	}
	if _, ok, _ := db.Acquire("evaluator", "copy-b", 15*time.Second, t0.Add(5*time.Second)); ok {
		t.Fatal("b took a lease a holds")
	}
	if a2, ok, _ := db.Acquire("evaluator", "copy-a", 15*time.Second, t0.Add(5*time.Second)); !ok || a2.Epoch != 1 {
		t.Fatalf("a renewing: %+v %v", a2, ok)
	}
	if h, _ := db.LeaseHolder("evaluator", t0.Add(10*time.Second)); h != "copy-a" {
		t.Fatalf("holder %q", h)
	}
	inc := Incident{ID: "B-0001", Key: "k", Severity: "warning", Title: "t", Opened: t0}
	if err := db.PutIncidentAs(inc, &a); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMetaAs("incident_seq", "1", &a); err != nil {
		t.Fatal(err)
	}

	// a stops renewing (paused); once the lease runs out b takes it.
	b, ok, _ := db.Acquire("evaluator", "copy-b", 15*time.Second, t0.Add(25*time.Second))
	if !ok || b.Epoch != 2 {
		t.Fatalf("b taking over: %+v %v", b, ok)
	}
	// a wakes: its writes are refused, and it can't renew.
	inc.Title = "from a stale evaluator"
	if err := db.PutIncidentAs(inc, &a); err != ErrFenced {
		t.Fatalf("stale write: %v", err)
	}
	if err := db.SetMetaAs("incident_seq", "9", &a); err != ErrFenced || db.Meta("incident_seq") != "1" {
		t.Fatalf("stale meta: %v %s", err, db.Meta("incident_seq"))
	}
	if _, ok, _ := db.Acquire("evaluator", "copy-a", 15*time.Second, t0.Add(26*time.Second)); ok {
		t.Fatal("a renewed a lease b holds")
	}
	inc.Title = "from b"
	if err := db.PutIncidentAs(inc, &b); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Incident("B-0001"); got.Title != "from b" {
		t.Fatalf("incident %+v", got)
	}
	// b leaves: a can take it straight away, at a new epoch.
	_ = db.Release(b)
	if a3, ok, _ := db.Acquire("evaluator", "copy-a", 15*time.Second, t0.Add(27*time.Second)); !ok || a3.Epoch != 3 {
		t.Fatalf("after release: %+v %v", a3, ok)
	}
}

func TestAckSurvivesTheEvaluatorsSave(t *testing.T) {
	db, _ := open(t)
	t0 := time.Unix(1_800_000_000, 0)
	inc := Incident{ID: "B-0001", Key: "k", Severity: "critical", Title: "t", Opened: t0, Fired: t0}
	_ = db.PutIncident(inc)
	_, _ = db.AckIncident("B-0001", "Pat", t0.Add(time.Minute)) // on another copy
	inc.Notified = t0.Add(2 * time.Minute)                      // the evaluator's next save
	_ = db.PutIncident(inc)
	if got, _ := db.Incident("B-0001"); got.AckBy != "Pat" {
		t.Fatalf("ack lost: %+v", got)
	}
	_ = db.ClearAck("B-0001")
	if got, _ := db.Incident("B-0001"); got.AckBy != "" {
		t.Fatalf("ack kept: %+v", got)
	}
	if _, err := db.Incident("B-0404"); err != ErrNotFound {
		t.Fatal(err)
	}
}
