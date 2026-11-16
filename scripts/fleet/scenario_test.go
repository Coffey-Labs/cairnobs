package main

import (
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestSpell(t *testing.T) {
	s := spell{every: time.Hour, at: 20 * time.Minute, on: 10 * time.Minute}
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at     time.Duration
		active bool
		began  time.Duration
	}{
		{19 * time.Minute, false, -40 * time.Minute},
		{20 * time.Minute, true, 20 * time.Minute},
		{29 * time.Minute, true, 20 * time.Minute},
		{30 * time.Minute, false, 20 * time.Minute},
		{80 * time.Minute, true, 80 * time.Minute},
	} {
		now := day.Add(c.at)
		if s.active(now) != c.active || !s.began(now).Equal(day.Add(c.began)) {
			t.Errorf("at %s: active %v began %s", c.at, s.active(now), s.began(now).Sub(day))
		}
	}
	if !always.active(day) || !always.began(day).Before(day) {
		t.Error("always")
	}
}

// The demo stays calm: across a day, no more than a few of the problems
// that come and go are on at once, and every one of them is on some of
// the time and off the rest.
func TestScenarioIsCalm(t *testing.T) {
	spells := map[string]spell{"replica": spellReplicaBehind, "legacy dc": spellLegacyDC, "redis": spellRedisFails, "app": spellAppExits,
		"502": spellLegacy502, "lb": spellLBFlaps, "crashloop": spellCrashloop, "esxi update": spellESXiUpdate, "esxi memory": spellESXiMemory,
		"ahv": spellAHVUpdate, "pve": spellPVEHA, "dr paths": spellDRPaths, "blade": spellBladeEvac, "vdi": spellVDIUpdate,
		"inquiry": spellInquiry, "orders": spellOrdersDown, "vault edge": spellVaultEdgeDown, "blocklist": spellBlocklisted,
		"late backup": spellLateBackup, "shrank": spellBackupShrank, "sql agent": spellSQLAgent, "fs crash": spellFSCrash, "activation": spellActivation}
	on := map[string]int{}
	most, total, steps := 0, 0, 0
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for m := 0; m < 24*60; m++ {
		now := day.Add(time.Duration(m) * time.Minute)
		n := 0
		for name, s := range spells {
			if s.on <= 0 || s.at+s.on > s.every {
				t.Fatalf("%s: on %s at %s of %s", name, s.on, s.at, s.every)
			}
			if s.active(now) {
				on[name]++
				n++
			}
		}
		most, total, steps = max(most, n), total+n, steps+1
	}
	for name := range spells {
		if on[name] == 0 || on[name] == 24*60 {
			t.Errorf("%s is on %d minutes of the day", name, on[name])
		}
	}
	if avg := float64(total) / float64(steps); avg > 4 || most > 9 {
		t.Errorf("on at once: %.1f on average, %d at most", avg, most)
	}
}

// A flapping record goes wrong during its spell and comes right after; a
// record only the bad snapshot has comes and goes.
func TestFlapSet(t *testing.T) {
	c := &fakeCluster{recs: map[string]report.Workload{}, changed: map[string]bool{}}
	good := []report.Workload{{ID: "a", State: report.StateOK, Word: "Active"}, {ID: "b", State: report.StateOK, Word: "Fine"}}
	bad := []report.Workload{{ID: "a", State: report.StateDown, Word: "Inactive"}, {ID: "b", State: report.StateOK, Word: "Fine"},
		{ID: "q", State: report.StateDegraded, Word: "14 jobs waiting"}}
	for _, r := range good {
		c.recs[r.ID] = r
	}
	s := spell{every: time.Hour, at: 0, on: 30 * time.Minute}
	c.flapSet(s, good, bad)
	if len(c.episodes) != 2 {
		t.Fatalf("%d episodes, want a and q", len(c.episodes))
	}
	hour := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	c.play(hour.Add(5 * time.Minute))
	if c.recs["a"].State != report.StateDown || c.recs["q"].ID != "q" {
		t.Fatalf("in the spell: %+v %+v", c.recs["a"], c.recs["q"])
	}
	c.removed = nil
	c.play(hour.Add(40 * time.Minute))
	if _, there := c.recs["q"]; c.recs["a"].State != report.StateOK || there || len(c.removed) != 1 {
		t.Fatalf("after: %+v, q there %v, removed %v", c.recs["a"], there, c.removed)
	}
}

// IBM i's flapping is only what the bad snapshot changes, not the whole
// partition.
func TestIBMiFlapsLittle(t *testing.T) {
	for k, part := range ibmiPartitions[:2] {
		c := newIBMi("fleet-0041", "t", k)
		if n := len(c.episodes); n == 0 || n > 4 {
			t.Errorf("%s: %d records flap", part, n)
		}
	}
}
