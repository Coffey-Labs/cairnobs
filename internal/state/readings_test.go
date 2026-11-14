package state

import (
	"testing"
	"time"
)

func TestReadingsKeepTheNewest(t *testing.T) {
	db, _ := open(t)
	if _, _, err := db.Reading("cloudflare"); err != ErrNotFound {
		t.Fatalf("none yet: %v", err)
	}
	now := time.Now().Truncate(time.Millisecond)
	if err := db.PutReading("cloudflare", []byte(`{"a":2}`), now); err != nil {
		t.Fatal(err)
	}
	_ = db.PutReading("cloudflare", []byte(`{"a":1}`), now.Add(-time.Second)) // late, from a copy that lost the lease
	b, at, err := db.Reading("cloudflare")
	if err != nil || string(b) != `{"a":2}` || !at.Equal(now) {
		t.Fatalf("%s %v %v", b, at, err)
	}
	_ = db.PutReading("mail", []byte(`{}`), now)
	if ts, err := db.ReadingTimes(); err != nil || len(ts) != 2 || !ts["mail"].Equal(now) {
		t.Fatalf("%v %v", ts, err)
	}
}
