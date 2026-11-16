package server

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

func TestStrips(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	got := strips([]state.Change{
		{Entity: "svc/a", At: at(-90), Status: "good"},    // as the day began
		{Entity: "svc/a", At: at(40), Status: "critical"}, // a blip inside cell 1
		{Entity: "svc/a", At: at(45), Status: "good"},     //
		{Entity: "svc/a", At: at(60), Status: "warning"},  // from cell 2's start
		{Entity: "svc/a", At: at(120), Status: "unknown"}, // obs-beacond down
		{Entity: "svc/a", At: at(170), Status: "good"},    // back in cell 5
		{Entity: "svc/b", At: at(100), Status: "good"},    // first known in cell 3
	}, t0, 6, 30*time.Minute)
	want := map[string]string{
		"svc/a": "good critical warning warning  good",
		"svc/b": "   good good good",
	}
	for e, w := range want {
		if g := strings.Join(got[e], " "); g != w {
			t.Errorf("%s = %q, want %q", e, g, w)
		}
	}
}

func TestToday(t *testing.T) {
	since := time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC)
	at := func(h float64) *time.Time { x := since.Add(time.Duration(h * float64(time.Hour))); return &x }
	open := []Incident{
		{ID: "B-10", Severity: "critical", Title: "drive failed", Opened: *at(4.5), Fired: at(4.5)},
		{ID: "B-9", Severity: "warning", Title: "leader moved", Opened: *at(3.8), Ack: &Ack{By: "sam", At: *at(4)}},
		{ID: "B-2", Severity: "warning", Title: "old and still open", Opened: *at(-30)},
	}
	recent := []Incident{
		{ID: "B-8", Severity: "warning", Title: "slow shop", Opened: *at(1), Resolved: at(1.2)},
		{ID: "B-7", Severity: "critical", Title: "closed overnight", Opened: *at(-1), Fired: at(-0.9), Resolved: at(0.5), Ack: &Ack{By: "ana"}},
		{ID: "B-1", Severity: "warning", Title: "long gone", Opened: *at(-20), Resolved: at(-19)},
	}
	got := today(open, recent, since)
	var ids []string
	for _, e := range got.Events {
		ids = append(ids, e.ID)
	}
	if j := strings.Join(ids, " "); j != "B-10 B-9 B-8 B-7" {
		t.Errorf("events = %s", j)
	}
	if got.Opened != 3 || got.Resolved != 2 || got.FixedItself != 1 || got.Told != 1 {
		t.Errorf("counts: opened %d resolved %d fixed itself %d told %d", got.Opened, got.Resolved, got.FixedItself, got.Told)
	}
	if e := got.Events[3]; !e.At.Equal(*at(0.5)) || e.AckBy != "ana" || e.Told {
		t.Errorf("B-7 (opened before, closed since) = %+v", e)
	}
	if e := got.Events[1]; e.AckBy != "sam" {
		t.Errorf("B-9 ack = %q", e.AckBy)
	}
	if empty := today(nil, nil, since); empty.Events == nil {
		t.Error("events should be [] not null")
	}
}

func TestVisited(t *testing.T) {
	srv := &Server{}
	t0 := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	if p := srv.visited("ana", t0); !p.IsZero() {
		t.Errorf("first visit: prev = %s", p)
	}
	if p := srv.visited("ana", t0.Add(10*time.Minute)); !p.IsZero() {
		t.Errorf("same visit: prev = %s", p)
	}
	// Away longer than the gap: the visit that ended at +10m is the previous one.
	if p := srv.visited("ana", t0.Add(5*time.Hour)); !p.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("next visit: prev = %s", p)
	}
	if p := srv.visited("ana", t0.Add(5*time.Hour+time.Minute)); !p.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("polling keeps the previous visit: prev = %s", p)
	}
	if p := srv.visited("sam", t0.Add(5*time.Hour)); !p.IsZero() {
		t.Errorf("another person: prev = %s", p)
	}
	if p, l := parseVisit("0 1763280000"); !p.IsZero() || l.Unix() != 1763280000 {
		t.Errorf("parseVisit = %s %s", p, l)
	}
}

func TestBusier(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 20, 0, 0, time.UTC)
	usual := map[string]*[24]usualHour{}
	for _, h := range []string{"calm", "hot", "hungry", "young"} {
		u := &[24]usualHour{}
		n := 3
		if h == "young" {
			n = 2 // two days of 14:00 aren't enough to say what's usual
		}
		u[14] = usualHour{n: n, cpu: 18 * float64(n), mem: 50 * float64(n)}
		usual[h] = u
	}
	got := busier([]HostView{
		{Name: "calm", CPU: 30, MemPct: 55},   // under twice usual, memory only 5 up
		{Name: "hot", CPU: 44, MemPct: 52},    // 44 ≥ 2×18 and 26 up
		{Name: "hungry", CPU: 10, MemPct: 81}, // memory 31 up
		{Name: "young", CPU: 90, MemPct: 90},
		{Name: "new", CPU: 90},
	}, usual, now)
	var s []string
	for _, b := range got {
		s = append(s, b.Host+"/"+b.What)
	}
	if j := strings.Join(s, " "); j != "hungry/memory hot/cpu" {
		t.Errorf("busier = %s", j)
	}
}

func TestRestartedAndFilling(t *testing.T) {
	hosts := []HostView{{Name: "a", Uptime: 3600}, {Name: "b", Uptime: 90000}, {Name: "c", Uptime: 600}, {Name: "d", Uptime: 60, Silent: true}}
	got, n := restarted(hosts)
	if n != 2 || got[0].Host != "c" || got[1].Host != "a" {
		t.Errorf("restarted = %+v (%d)", got, n)
	}
	t0 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	var hours []state.HostHour
	for i := range 48 {
		h := t0.Add(time.Duration(i) * time.Hour)
		hours = append(hours,
			state.HostHour{Host: "fills", Hour: h, Disks: []state.DiskSample{{Mount: "/var", Used: uint64(600 + i*2), Total: 1000}}}, // 2 a hour, 306 left: ~6 days
			state.HostHour{Host: "steady", Hour: h, Disks: []state.DiskSample{{Mount: "/", Used: 900, Total: 1000}}},
			state.HostHour{Host: "slow", Hour: h, Disks: []state.DiskSample{{Mount: "/", Used: uint64(500 + i/12), Total: 1000}}}, // years
		)
	}
	f := fillingDisks(hours)
	if len(f) != 1 || f[0].Host != "fills" || f[0].FullDays < 6 || f[0].FullDays > 7 {
		t.Errorf("filling = %+v", f)
	}
}

func TestWithIncidents(t *testing.T) {
	attn := []Attention{{Key: "disk/a//", Title: "disk"}, {Key: "", Title: "folded"}, {Key: "unit/b/x", Title: "no incident yet"}}
	open := []Incident{{ID: "B-3", Key: "disk/a//", Ack: &Ack{By: "ana"}}, {ID: "B-4", Key: ""}}
	got := withIncidents(attn, open)
	if got[0].Incident == nil || got[0].Incident.ID != "B-3" || got[0].Incident.Ack.By != "ana" {
		t.Errorf("disk: %+v", got[0].Incident)
	}
	if got[1].Incident != nil || got[2].Incident != nil {
		t.Errorf("others: %+v %+v", got[1].Incident, got[2].Incident)
	}
}
