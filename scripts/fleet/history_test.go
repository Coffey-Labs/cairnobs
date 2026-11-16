package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

// Hosts stay under the warnings (disks 80%, memory 90%), but phx1-app20's
// log volume, which crosses 80% for a while every ten days (#40).
func TestLoadStaysCalm(t *testing.T) {
	now := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)
	over := 0
	for i := range 150 {
		name := hostName(i)
		for h := range 30 * 24 {
			at := now.Add(-time.Duration(h) * time.Hour)
			if c := cpuNow(name, at); c <= 0 || c > 100 {
				t.Fatalf("%s at %s: CPU %.1f", name, at, c)
			}
			if m := memAt(name, at); m >= 90 {
				t.Fatalf("%s at %s: memory %.1f", name, at, m)
			}
			for k := range 2 {
				if d := diskAt(name, k, at); d >= 0.8 {
					if i != 23 || k != 1 {
						t.Fatalf("%s disk %d at %s: %.2f", name, k, at, d)
					}
					over++
				}
			}
		}
	}
	if over == 0 || over > 30*24/3 {
		t.Errorf("phx1-app20's log volume is over 80%% for %d of 720 hours", over)
	}
	// Not all alike, as every disk at 33% was.
	if diskAt(hostName(1), 0, now) == diskAt(hostName(2), 0, now) {
		t.Error("two hosts' disks are the same")
	}
}

func TestWriteHistory(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "beacon.db")
	hosts := []*host{newHost(1, hostName(1), 2, 10*time.Second), newHost(2, hostName(2), 2, 10*time.Second)}
	now := time.Now()
	if err := writeHistory(db, key, 3, pastFleet{hosts: hosts, vaults: true, mail: true, checks: []string{"web-legacy"}}, now); err != nil {
		t.Fatal(err)
	}
	d, err := state.Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	hs, _ := d.HostHours(now.Add(-4*24*time.Hour), now)
	if len(hs) != 2*3*24 {
		t.Errorf("%d host hours", len(hs))
	}
	incs, _ := d.IncidentsBetween(now.Add(-4*24*time.Hour), now)
	if len(incs) < 20 || d.Meta("incident_seq") != fmt.Sprint(len(incs)) {
		t.Errorf("%d incidents, incident_seq %s", len(incs), d.Meta("incident_seq"))
	}
	for _, i := range incs {
		if i.Resolved.After(now) || !i.Resolved.After(i.Opened) {
			t.Fatalf("%s %s: opened %s, resolved %s", i.ID, i.Key, i.Opened, i.Resolved)
		}
	}
	if d.HistoryStart().IsZero() {
		t.Error("no statuses")
	}
	// A second run leaves it alone.
	if err := writeHistory(db, key, 3, pastFleet{hosts: hosts}, now); err != nil {
		t.Fatal(err)
	}
	if hs2, _ := d.HostHours(now.Add(-4*24*time.Hour), now); len(hs2) != len(hs) {
		t.Errorf("written twice: %d hours", len(hs2))
	}
}
