package state

import (
	"testing"
	"time"
)

func TestEdgesBecomeSteady(t *testing.T) {
	db, _ := open(t)
	t0 := time.Date(2026, 11, 13, 20, 10, 0, 0, time.UTC)
	k := EdgeKey{"compose:webmail", "unit:patroni", 5432}
	other := EdgeKey{"unit:nginx", "compose:webmail", 8080}
	seen := func(at time.Time, keys ...EdgeKey) {
		t.Helper()
		if err := db.SeenEdges(keys, at); err != nil {
			t.Fatal(err)
		}
	}
	row := func(k EdgeKey) EdgeRow {
		t.Helper()
		rows, err := db.Edges()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.EdgeKey == k {
				return r
			}
		}
		t.Fatalf("no row for %v", k)
		return EdgeRow{}
	}
	seen(t0, k, other)
	seen(t0.Add(20*time.Minute), k) // the same hour: still one
	seen(t0.Add(2*time.Hour), k)
	if r := row(k); len(r.Hours) != 2 || !r.Steady.IsZero() || !r.First.Equal(t0) || !r.Last.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("after two hours: %+v", r)
	}
	seen(t0.Add(-time.Hour), k) // late, from a copy that lost the lease: the last stays
	seen(t0.Add(30*time.Hour), k)
	r := row(k)
	if r.Steady.IsZero() || !r.Last.Equal(t0.Add(30*time.Hour)) {
		t.Fatalf("three hours within a week: %+v", r)
	}
	// Three hours spread over more than a week aren't steady.
	seen(t0.Add(8*24*time.Hour), other)
	seen(t0.Add(16*24*time.Hour), other)
	if r := row(other); !r.Steady.IsZero() || len(r.Hours) != 3 {
		t.Fatalf("spread out: %+v", r)
	}
	if n, err := db.PurgeEdges(t0.Add(10 * 24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if rows, _ := db.Edges(); len(rows) != 1 || rows[0].EdgeKey != other {
		t.Fatalf("left: %+v", rows)
	}
}

func TestServicesFirstSeen(t *testing.T) {
	db, _ := open(t)
	t0 := time.Date(2026, 11, 13, 20, 10, 0, 0, time.UTC)
	if err := db.SeenServices([]string{"compose:webmail", "unit:patroni"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := db.SeenServices([]string{"compose:webmail"}, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SeenServices([]string{"compose:webmail"}, t0.Add(time.Hour)); err != nil { // late: the last stays
		t.Fatal(err)
	}
	rows, err := db.MapServices()
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %+v", err, rows)
	}
	for _, r := range rows {
		if r.ID == "compose:webmail" && (!r.First.Equal(t0) || !r.Last.Equal(t0.Add(3*time.Hour))) {
			t.Errorf("webmail: %+v", r)
		}
	}
	if n, err := db.PurgeServices(t0.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
}
