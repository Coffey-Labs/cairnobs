package state

import (
	"testing"
	"time"
)

func TestActionsTakenOnce(t *testing.T) {
	db, _ := open(t)
	now := time.Now().Truncate(time.Second)
	for i, a := range []Action{
		{ID: "a1", Agent: "web1", Kind: "reload", Args: `{"unit":"nginx.service"}`, By: "john", Created: now.Add(-time.Minute)},
		{ID: "a2", Agent: "web1", Kind: "reload", Args: `{}`, By: "john", Created: now.Add(-20 * time.Minute)}, // too old to hand over
		{ID: "a3", Agent: "web2", Kind: "reload", Args: `{}`, By: "john", Created: now},
	} {
		if err := db.AddAction(a); err != nil {
			t.Fatal(i, err)
		}
	}
	since := now.Add(-10 * time.Minute)
	if w, err := db.ActionsWaiting("web1", since); err != nil || !w {
		t.Fatalf("waiting: %v %v", w, err)
	}
	got, err := db.TakeActions("web1", since, now)
	if err != nil || len(got) != 1 || got[0].ID != "a1" || got[0].Args != `{"unit":"nginx.service"}` || got[0].By != "john" {
		t.Fatalf("%+v %v", got, err)
	}
	if again, _ := db.TakeActions("web1", since, now); len(again) != 0 {
		t.Fatalf("handed over twice: %+v", again)
	}
	if w, _ := db.ActionsWaiting("web1", since); w {
		t.Error("still waiting after it was taken")
	}
	if ok, err := db.FinishAction("a1", "web2", true, "", now); ok || err != nil {
		t.Errorf("another agent finished it: %v %v", ok, err)
	}
	if ok, err := db.FinishAction("a3", "web2", true, "", now); ok || err != nil {
		t.Errorf("finished before it was taken: %v %v", ok, err)
	}
	if ok, err := db.FinishAction("a1", "web1", true, "reloaded", now); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := db.FinishAction("a1", "web1", false, "again", now); ok {
		t.Error("finished twice")
	}
	all, err := db.Actions(10)
	if err != nil || len(all) != 3 || all[0].ID != "a3" || all[1].ID != "a1" || !all[1].OK || all[1].Detail != "reloaded" || all[1].Done.IsZero() || !all[2].Taken.IsZero() {
		t.Fatalf("%+v %v", all, err)
	}
}
