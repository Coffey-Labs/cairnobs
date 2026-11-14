package state

import (
	"testing"
	"time"
)

func TestWorkloadsListingAndChanges(t *testing.T) {
	db, _ := open(t)
	rec := func(id, body string) WorkloadRecord { return WorkloadRecord{ID: id, Kind: "pod", Record: []byte(body)} }
	now := time.Now()
	put := func(b WorkloadBatch) {
		t.Helper()
		b.Source, b.Agent, b.Heard = "prod", "0.2.0", now
		if err := db.PutWorkloads(b); err != nil {
			t.Fatal(err)
		}
	}
	put(WorkloadBatch{Generation: "g1", Full: "begin", Records: []WorkloadRecord{rec("a", `{"s":1}`), rec("b", `{"s":1}`)}})
	put(WorkloadBatch{Generation: "g1", Full: "end", Records: []WorkloadRecord{rec("c", `{"s":1}`), rec("c", `{"s":2}`)}})
	all, err := db.WorkloadsSince(-1)
	if err != nil || len(all) != 3 {
		t.Fatalf("after the first listing: %d rows, %v", len(all), err)
	}
	src, _ := db.WorkloadSources()
	if len(src) != 1 || src[0].Listed.IsZero() || src[0].Generation != "g1" {
		t.Fatalf("source: %+v", src)
	}
	mark, _ := db.DBNowMS()
	time.Sleep(5 * time.Millisecond)
	// A change, a removal, and an unchanged record that isn't read again.
	put(WorkloadBatch{Generation: "g1", Records: []WorkloadRecord{rec("a", `{"s":2}`), rec("b", `{"s":1}`)}, Removed: []string{"c"}})
	ch, _ := db.WorkloadsSince(mark)
	if len(ch) != 2 {
		t.Fatalf("changed since: %+v", ch)
	}
	for _, w := range ch {
		if (w.ID == "a" && (w.Gone || string(w.Record) != `{"s":2}`)) || (w.ID == "c" && !w.Gone) || w.ID == "b" {
			t.Fatalf("change: %+v", w)
		}
	}
	// A new listing without b: b is gone.
	mark, _ = db.DBNowMS()
	time.Sleep(5 * time.Millisecond)
	put(WorkloadBatch{Generation: "g2", Full: "begin", Records: []WorkloadRecord{rec("a", `{"s":2}`)}})
	put(WorkloadBatch{Generation: "g2", Full: "end"})
	ch, _ = db.WorkloadsSince(mark)
	if len(ch) != 1 || ch[0].ID != "b" || !ch[0].Gone {
		t.Fatalf("after the second listing: %+v", ch)
	}
	if all, _ := db.WorkloadsSince(-1); len(all) != 1 {
		t.Fatalf("left: %+v", all)
	}
	if n, err := db.PurgeWorkloads(mark + 60_000); err != nil || n != 2 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if err := db.DeleteWorkloadSource("prod"); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.WorkloadSources(); len(s) != 0 {
		t.Fatalf("sources left: %+v", s)
	}
}
