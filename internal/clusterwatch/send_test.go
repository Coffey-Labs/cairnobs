package clusterwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// sink records what the sender sends, and can refuse.
type sink struct {
	got  []report.Workloads
	fail bool
}

func (s *sink) send(_ context.Context, b report.Workloads) error {
	if s.fail {
		return errors.New("obs-beacond is away")
	}
	s.got = append(s.got, b)
	return nil
}

func (s *sink) take() []report.Workloads {
	g := s.got
	s.got = nil
	return g
}

func podJSON(uid, phase string) string {
	ready := "false"
	if phase == "Running" {
		ready = "true"
	}
	return `{"metadata":{"uid":"` + uid + `","name":"` + uid + `","namespace":"a","creationTimestamp":"2026-11-13T09:00:00Z"},
		"spec":{"nodeName":"w1"},"status":{"phase":"` + phase + `","containerStatuses":[{"name":"c","ready":` + ready + `}]}}`
}

func TestSenderListsThenSendsChanges(t *testing.T) {
	st := newStore()
	for i := range 5 {
		st.objs.add(t, "pods", podJSON(string(rune('a'+i)), "Running"))
	}
	out := &sink{}
	sn := newSender(Config{Name: "prod", Page: 2}, out.send)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	// First: a full listing, in pages of 2 (5 pods and the cluster: 3 pages), then the end.
	sn.tick(context.Background(), st, now)
	got := out.take()
	if len(got) != 4 || got[0].Full != report.FullBegin || got[1].Full != report.FullPage || got[3].Full != report.FullEnd || len(got[3].Records) != 0 {
		t.Fatalf("full listing: %d batches %+v", len(got), got)
	}
	gen := got[0].Generation
	for _, b := range got {
		if b.Generation != gen || b.V != report.Version {
			t.Fatalf("generation or version: %+v", b)
		}
	}
	// Nothing changed, within the heartbeat: nothing sent.
	sn.tick(context.Background(), st, now.Add(5*time.Second))
	if g := out.take(); len(g) != 0 {
		t.Fatalf("sent with nothing changed: %+v", g)
	}
	// One pod crashes, one goes: one batch with the change and the removal.
	st.objs.add(t, "pods", `{"metadata":{"uid":"a","name":"a","namespace":"a"},"spec":{"nodeName":"w1"},"status":{"phase":"Running","containerStatuses":[{"name":"c","state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`)
	delete(st.objs["pods"], "b")
	at := now.Add(10 * time.Second)
	sn.tick(context.Background(), st, at)
	got = out.take()
	if len(got) != 1 || got[0].Full != "" || len(got[0].Records) != 1 || got[0].Records[0].State != report.StateCrashloop ||
		len(got[0].Removed) != 1 || got[0].Removed[0] != "b" || got[0].Generation != gen {
		t.Fatalf("change: %+v", got)
	}
	if !got[0].Records[0].Since.Equal(at) {
		t.Fatalf("since: %s, want when the change was seen", got[0].Records[0].Since)
	}
	// It stays crashing: Since stays.
	sn.tick(context.Background(), st, at.Add(30*time.Second))
	if got = out.take(); len(got) != 1 || len(got[0].Records) != 0 {
		t.Fatalf("heartbeat: %+v", got)
	}
	// obs-beacond is away for 3 minutes; a change meanwhile is sent when
	// it's back, and the next tick lists everything again.
	out.fail = true
	st.objs.add(t, "pods", podJSON("f", "Pending"))
	for i := 1; i <= 36; i++ {
		sn.tick(context.Background(), st, at.Add(30*time.Second+time.Duration(i)*5*time.Second))
	}
	out.fail = false
	back := at.Add(4 * time.Minute)
	sn.tick(context.Background(), st, back)
	if got = out.take(); len(got) != 1 || len(got[0].Records) != 1 || got[0].Records[0].ID != "f" {
		t.Fatalf("after the gap: %+v", got)
	}
	sn.tick(context.Background(), st, back.Add(5*time.Second))
	got = out.take()
	if len(got) == 0 || got[0].Full != report.FullBegin || got[0].Generation == gen {
		t.Fatalf("no new full listing after the gap: %+v", got)
	}
	var crash report.Workload
	for _, b := range got {
		for _, r := range b.Records {
			if r.ID == "a" {
				crash = r
			}
		}
	}
	if !crash.Since.Equal(at) {
		t.Fatalf("since across the listing: %s, want %s", crash.Since, at)
	}
}
