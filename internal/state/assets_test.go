package state

import (
	"testing"
	"time"
)

// Each version's files are kept for the next few, so old and new copies
// can serve each other's pages during a rollout.
func TestAssetsKeptAcrossVersions(t *testing.T) {
	db, _ := open(t)
	t0 := time.Unix(1792000000, 0)
	put := func(v string, at time.Duration, files ...string) {
		t.Helper()
		m := map[string][]byte{}
		for _, f := range files {
			m[f] = []byte(v + ":" + f)
		}
		if err := db.PutAssets(v, m, t0.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	put("v1", 0, "_app/immutable/a.js", "_app/immutable/shared.css")
	put("v2", time.Hour, "_app/immutable/b.js", "_app/immutable/shared.css")
	put("v2", 2*time.Hour, "_app/immutable/b.js", "_app/immutable/shared.css") // a second copy of v2 starting
	if b, err := db.Asset("_app/immutable/a.js"); err != nil || string(b) != "v1:_app/immutable/a.js" {
		t.Fatalf("v1's file on v2: %q %v", b, err)
	}
	if _, err := db.Asset("_app/immutable/nope.js"); err != ErrNotFound {
		t.Fatalf("missing file: %v", err)
	}
	// A rebuilt version replaces its own files.
	put("v2", 3*time.Hour, "_app/immutable/b2.js")
	if _, err := db.Asset("_app/immutable/b.js"); err != ErrNotFound {
		t.Fatalf("a rebuilt version's old file stayed: %v", err)
	}
	// Only the newest three versions are kept.
	put("v3", 4*time.Hour, "_app/immutable/c.js")
	put("v4", 5*time.Hour, "_app/immutable/d.js")
	if _, err := db.Asset("_app/immutable/a.js"); err != ErrNotFound {
		t.Fatalf("v1 kept after three newer: %v", err)
	}
	for _, p := range []string{"_app/immutable/b2.js", "_app/immutable/c.js", "_app/immutable/d.js"} {
		if _, err := db.Asset(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	// An old version starting again (a rollback) is newest again, and keeps its files.
	put("v1", 6*time.Hour, "_app/immutable/a.js")
	if _, err := db.Asset("_app/immutable/b2.js"); err != ErrNotFound {
		t.Fatalf("v2 kept after the rollback made it fourth: %v", err)
	}
}
