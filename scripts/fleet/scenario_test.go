package main

import (
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A spell comes round about every period, never like a clock (#57), and
// active, last and between always agree.
func TestSpell(t *testing.T) {
	s := spell{every: time.Hour, at: 20 * time.Minute, on: 10 * time.Minute}
	day := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC) // midnight in Phoenix
	week := s.between(day, day.Add(7*24*time.Hour))
	starts := map[time.Duration]bool{}
	night, nights, daytime, days := 0, 0, 0, 0
	for i, o := range week {
		if l := o.end.Sub(o.start); l < 7*time.Minute || l > 13*time.Minute {
			t.Errorf("%s lasts %s", o.start, l)
		}
		if s.period(o.start) != s.period(o.end.Add(-time.Nanosecond)) {
			t.Errorf("%s runs into the next period", o.start)
		}
		if i > 0 && !week[i-1].end.Before(o.start) {
			t.Errorf("%s overlaps the one before", o.start)
		}
		starts[time.Duration(o.start.UnixNano())%s.every/time.Minute] = true
		mid := o.start.Add(o.end.Sub(o.start) / 2)
		if !s.active(mid) || !s.last(mid).start.Equal(o.start) || !s.began(o.end).Equal(o.start) || s.active(o.end) {
			t.Errorf("%s: active %v last %s", mid, s.active(mid), s.last(mid).start)
		}
		if hr := o.start.In(phoenix).Hour(); hr < 7 || hr >= 22 {
			night++
		} else {
			daytime++
		}
	}
	for h := 0; h < 7*24; h++ {
		if hr := day.Add(time.Duration(h) * time.Hour).In(phoenix).Hour(); hr < 7 || hr >= 22 {
			nights++
		} else {
			days++
		}
	}
	if n := len(week); n < 7*24/5 || n > 7*24*2/3 {
		t.Errorf("%d of %d hours had it", n, 7*24)
	}
	if float64(night)/float64(nights) >= float64(daytime)/float64(days) {
		t.Errorf("as often at night (%d/%d) as by day (%d/%d)", night, nights, daytime, days)
	}
	if len(starts) < 5 {
		t.Errorf("starts at only %d minutes of the hour", len(starts))
	}
	// Between spells, the last one is the one before.
	for i := 1; i < len(week); i++ {
		gap := week[i-1].end.Add(time.Second)
		if gap.Before(week[i].start) && (s.active(gap) || !s.last(gap).start.Equal(week[i-1].start)) {
			t.Errorf("%s: active %v last %s", gap, s.active(gap), s.last(gap).start)
		}
	}
	if !always.active(day) || !always.began(day).Before(day) || always.between(day, day.Add(time.Hour)) != nil {
		t.Error("always")
	}
}

// The demo stays calm: across three days, no more than a few of the
// problems that come and go are on at once, every one of them is on some
// of the time and off the rest, and in the Phoenix day a visitor never
// goes an hour without one.
func TestScenarioIsCalm(t *testing.T) {
	spells := map[string]spell{"replica": spellReplicaBehind, "legacy dc": spellLegacyDC, "redis": spellRedisFails, "app": spellAppExits,
		"502": spellLegacy502, "lb": spellLBFlaps, "crashloop": spellCrashloop, "esxi update": spellESXiUpdate, "esxi memory": spellESXiMemory,
		"ahv": spellAHVUpdate, "pve": spellPVEHA, "dr paths": spellDRPaths, "blade": spellBladeEvac, "vdi": spellVDIUpdate,
		"inquiry": spellInquiry, "orders": spellOrdersDown, "vault edge": spellVaultEdgeDown, "blocklist": spellBlocklisted,
		"late backup": spellLateBackup, "shrank": spellBackupShrank, "sql agent": spellSQLAgent, "fs crash": spellFSCrash, "activation": spellActivation}
	on := map[string]int{}
	most, total, steps := 0, 0, 0
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	const minutes = 3 * 24 * 60
	quiet, longest := 0, 0
	for m := 0; m < minutes; m++ {
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
		if hr := now.In(phoenix).Hour(); n == 0 && hr >= 7 && hr < 22 {
			quiet++
			longest = max(longest, quiet)
		} else {
			quiet = 0
		}
	}
	for name := range spells {
		if on[name] == 0 || on[name] == minutes {
			t.Errorf("%s is on %d minutes of three days", name, on[name])
		}
	}
	t.Logf("longest quiet daytime stretch %d min; %.1f on at once on average, %d at most", longest, float64(total)/float64(steps), most)
	if longest >= 60 {
		t.Errorf("%d minutes in the day with nothing going on", longest)
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
	o := s.between(time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC))[0]
	c.play(o.start.Add(time.Minute))
	if c.recs["a"].State != report.StateDown || c.recs["q"].ID != "q" {
		t.Fatalf("in the spell: %+v %+v", c.recs["a"], c.recs["q"])
	}
	c.removed = nil
	c.play(o.end.Add(time.Minute))
	if _, there := c.recs["q"]; c.recs["a"].State != report.StateOK || there || len(c.removed) != 1 {
		t.Fatalf("after: %+v, q there %v, removed %v", c.recs["a"], there, c.removed)
	}
}

// IBM i's flapping is only what the bad snapshot changes, not the whole
// partition.
func TestIBMiFlapsLittle(t *testing.T) {
	for k, part := range ibmiPartitions[:2] {
		c := newIBMi(ibmiCollector.name, "t", k)
		if n := len(c.episodes); n == 0 || n > 4 {
			t.Errorf("%s: %d records flap", part, n)
		}
	}
}
