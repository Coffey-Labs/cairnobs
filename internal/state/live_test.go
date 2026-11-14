package state

import (
	"context"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func TestLiveKeepsTheNewestReport(t *testing.T) {
	db, _ := open(t)
	t0 := time.UnixMilli(1_800_000_000_123)
	if err := db.PutLive("web1", []byte(`{"v":2}`), t0); err != nil {
		t.Fatal(err)
	}
	_ = db.PutLive("web1", []byte(`{"v":1}`), t0.Add(-time.Second)) // late, from another copy
	rep, at, err := db.Live("web1")
	if err != nil || string(rep) != `{"v":2}` || !at.Equal(t0) {
		t.Fatalf("%s %s %v", rep, at, err)
	}
	_ = db.PutLive("web1", []byte(`{"v":3}`), t0.Add(time.Second))
	if rep, _, _ := db.Live("web1"); string(rep) != `{"v":3}` {
		t.Fatalf("newer not kept: %s", rep)
	}
	if times, _ := db.LiveTimes(); len(times) != 1 || !times["web1"].Equal(t0.Add(time.Second)) {
		t.Fatalf("times %v", times)
	}
	_ = db.DeleteLive("web1")
	if _, _, err := db.Live("web1"); err != ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
}

func TestSessionByID(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	_ = db.PutSession(Session{TokenSHA256: "s1", User: "Pat", Role: "admin", Dev: true, Expires: now.Add(time.Hour), Checked: now, Sealed: db.Seal([]byte("x"))})
	_ = db.PutSession(Session{TokenSHA256: "s2", User: "Old", Expires: now.Add(-time.Hour), Checked: now, Sealed: db.Seal([]byte("x"))})
	if s, err := db.Session("s1", now); err != nil || s.User != "Pat" || !s.Dev {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := db.Session("s2", now); err != ErrNotFound {
		t.Fatalf("expired: %v", err)
	}
	if ids, _ := db.SessionIDs(); !ids["s1"] || len(ids) != 2 {
		t.Fatalf("ids %v", ids)
	}
}

func TestBusCarriesNotices(t *testing.T) {
	postgresOnly(t)
	db, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	heard := make(chan string, 4)
	go db.Listen(ctx, func() { ready <- struct{}{} }, func(p string) { heard <- p })
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("not listening")
	}
	if err := db.Notify("copy1\thost\tweb1"); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-heard:
		if p != "copy1\thost\tweb1" {
			t.Fatalf("heard %q", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing heard")
	}
}

func TestBusIsQuietOnSQLite(t *testing.T) {
	statetest.SQLiteOnly(t)
	db, _ := open(t)
	done := make(chan struct{})
	go func() { db.Listen(context.Background(), func() { t.Error("ready on SQLite") }, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen blocked on SQLite")
	}
	if err := db.Notify("x"); err != nil || db.Shared() {
		t.Fatal(err)
	}
}

// Two installs in one PostgreSQL database, in two schemas, don't hear each
// other.
func TestBusIsPerSchema(t *testing.T) {
	postgresOnly(t)
	a, _ := open(t)
	b, _ := open(t)
	if a.channel == b.channel || a.channel == "" {
		t.Fatalf("channels %q and %q", a.channel, b.channel)
	}
}
