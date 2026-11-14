package maint

import (
	"testing"
	"time"
)

func TestDailyWindowInItsTimezone(t *testing.T) {
	w := Window{Name: "host1 reboot", Daily: "04:30", Timezone: "Europe/Berlin", Duration: 20 * time.Minute}
	if err := w.Check(); err != nil {
		t.Fatal(err)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	at := func(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, berlin) }
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{at(2026, 10, 16, 4, 29), false},
		{at(2026, 10, 16, 4, 30), true},
		{at(2026, 10, 16, 4, 49), true},
		{at(2026, 10, 16, 4, 50), false},
		// After the clocks go back (25 Oct) it is still 04:30 Berlin time: 03:30 UTC, not 02:30.
		{time.Date(2026, 10, 26, 3, 35, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 26, 2, 35, 0, 0, time.UTC), false},
	} {
		if got := w.Active(c.t); got != c.want {
			t.Errorf("%v: active %v, want %v", c.t, got, c.want)
		}
	}
	// 19:00 MST on the 15th is 04:00 Berlin on the 16th; next opens 30 minutes later.
	phx, _ := time.LoadLocation("America/Phoenix")
	now := time.Date(2026, 10, 15, 19, 0, 0, 0, phx)
	if next := w.Next(now); !next.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("next %v", next)
	}
}

func TestPastMidnight(t *testing.T) {
	w := Window{Name: "late", Daily: "23:50", Duration: 30 * time.Minute}
	if err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if !w.Active(time.Date(2026, 10, 16, 0, 10, 0, 0, time.UTC)) || w.Active(time.Date(2026, 10, 16, 0, 25, 0, 0, time.UTC)) {
		t.Fatal("window across midnight")
	}
}

func TestBadWindows(t *testing.T) {
	for _, w := range []Window{
		{Daily: "4:30", Duration: time.Minute},
		{Daily: "24:00", Duration: time.Minute},
		{Daily: "04:30"},
		{Daily: "04:30", Duration: time.Minute, Timezone: "Mars/Olympus"},
	} {
		if err := w.Check(); err == nil {
			t.Errorf("accepted %+v", w)
		}
	}
}
