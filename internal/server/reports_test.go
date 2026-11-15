package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

func TestIntegrate(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := time.Hour
	got := integrate([]state.Change{
		{Entity: "svc/mail", At: t0.Add(-5 * h), Status: "good"}, // as the period began
		{Entity: "svc/mail", At: t0.Add(2 * h), Status: "critical"},
		{Entity: "svc/mail", At: t0.Add(3 * h), Status: "warning"},
		{Entity: "svc/mail", At: t0.Add(4 * h), Status: "unknown"}, // obs-beacond down
		{Entity: "svc/mail", At: t0.Add(6 * h), Status: "good"},
		{Entity: "svc/git", At: t0.Add(9 * h), Status: "good"}, // first seen late
	}, t0, t0.Add(10*h))
	m := got["svc/mail"]
	if m.KnownS != 8*3600 || m.DownS != 3600 || m.DegradedS != 3600 || math.Abs(m.Pct-87.5) > 1e-9 {
		t.Errorf("mail = %+v", m)
	}
	if g := got["svc/git"]; g.KnownS != 3600 || g.Pct != 100 {
		t.Errorf("git = %+v", g)
	}
}

func TestCapacityForecast(t *testing.T) {
	loc, _ := time.LoadLocation("America/Phoenix")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const gb = 1 << 30
	var hours []state.HostHour
	for i := 0; i < 48; i++ {
		used := uint64(50*gb + i*gb/24) // a GB a day
		hours = append(hours, state.HostHour{Host: "node1", Hour: t0.Add(time.Duration(i) * time.Hour), CPU: 10, CPUMax: 20, Mem: 40,
			Disks: []state.DiskSample{{Mount: "/", Used: used, Total: 100 * gb}, {Mount: "/boot", Used: gb, Total: 2 * gb}}})
	}
	disks, trends := capacity(hours, loc)
	if len(disks) != 2 || disks[0].Mount != "/" {
		t.Fatalf("%+v", disks)
	}
	d := disks[0]
	if math.Abs(d.PerDay-gb) > gb/100 || d.FullDays == nil || math.Abs(*d.FullDays-48) > 1 || d.Projected == "" {
		t.Errorf("/ = %+v (full in %v)", d, d.FullDays)
	}
	if disks[1].FullDays != nil {
		t.Errorf("/boot isn't growing: %+v", disks[1])
	}
	if len(trends) != 1 || len(trends[0].Days) != 3 || trends[0].CPU[0] != 10 { // 48 UTC hours span three Phoenix days
		t.Errorf("trends = %+v", trends)
	}
}

func TestPeriod(t *testing.T) {
	loc, _ := time.LoadLocation("America/Phoenix")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, loc)
	from, to, label, err := period("2026-09", now, loc)
	if err != nil || !from.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, loc)) || !to.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, loc)) || label != "September 2026" {
		t.Errorf("%s %s %q %v", from, to, label, err)
	}
	if _, to, _, _ := period("2026-10", now, loc); !to.Equal(now) {
		t.Errorf("this month should end now, not %s", to)
	}
	if _, _, _, err := period("last week", now, loc); err == nil {
		t.Error("want an error")
	}
}

func TestReportAndDigest(t *testing.T) {
	srv := stateServer(t, t.TempDir(), true)
	srv.cfg.Reports = &ReportsConfig{Digest: &DigestConfig{}}
	if err := srv.cfg.Reports.check(); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Alerts.PublicURL = "https://beacon.example.test"
	now := time.Now()
	start := now.Add(-3 * time.Hour)
	srv.started = start.Add(-time.Hour)
	rec := &recorder{last: map[string]string{}, samples: map[string]*hourSum{}}

	// node1 reports for two hours, goes silent for one.
	for m := 0; m <= 120; m++ {
		at := start.Add(time.Duration(m) * time.Minute)
		srv.store.Put("node1", report.Report{V: report.Version, Time: at, CPU: 5, Mem: report.Mem{Total: 100, Available: 60},
			Disks: []report.Disk{{Mount: "/", Size: 1000, Used: 400, Avail: 600}}}, at)
		srv.recordOnce(rec, at)
	}
	srv.recordOnce(rec, start.Add(150*time.Minute)) // silent by now
	_ = srv.db.PutIncident(state.Incident{ID: "a1", Key: "silent/node1", Severity: "critical", Title: "node1 is silent", Host: "node1",
		Opened: start.Add(122 * time.Minute), Fired: start.Add(124 * time.Minute), AckBy: "John", AckAt: start.Add(130 * time.Minute)})
	_ = srv.db.SetNote("a1", state.Note{Text: "rebooted by hand", By: "John", At: now})
	_ = srv.db.PutIncident(state.Incident{ID: "a2", Key: "x", Severity: "warning", Title: "held back, never told", Opened: start})

	r, err := srv.buildReport(start, now, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hosts) != 1 || r.Hosts[0].ID != "node1" || math.Abs(r.Hosts[0].DownS-1800) > 60 || math.Abs(r.Hosts[0].KnownS-3*3600) > 120 || r.Hosts[0].Incidents != 1 {
		t.Errorf("hosts = %+v", r.Hosts)
	}
	if len(r.Incidents) != 1 || r.Incidents[0].Note == nil || *r.Incidents[0].ToAckS != 360 {
		t.Errorf("incidents = %+v", r.Incidents)
	}
	hh, _ := srv.db.HostHours(start.Add(-time.Hour), now)
	if len(hh) == 0 || hh[0].Disks[0].Used != 400 {
		t.Errorf("host hours = %+v", hh)
	}

	subject, text, short, err := srv.digest(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"node1 is silent", "fixed in", "still open", "cause: rebooted by hand", "https://beacon.example.test/reports?period=7d"} {
		if want == "fixed in" {
			continue // a1 isn't resolved
		}
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(subject, "Weekly digest") || !strings.Contains(short, "1 incident") {
		t.Errorf("subject %q short %q", subject, short)
	}

	// CSV.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/reports/csv/incidents?period=7d", nil)
	req.SetPathValue("section", "incidents")
	srv.handleReportCSV(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "rebooted by hand") || !strings.HasPrefix(w.Body.String(), "id,opened,") {
		t.Errorf("csv %d: %s", w.Code, w.Body)
	}
	// The JSON page lists the months there's history for.
	w = httptest.NewRecorder()
	srv.handleReports(w, httptest.NewRequest("GET", "/api/v1/reports?period=7d", nil))
	var v struct {
		Months []string `json:"months"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if len(v.Months) == 0 || v.Months[0] != now.In(srv.reportsLoc()).Format("2006-01") {
		t.Errorf("months = %v", v.Months)
	}
	// A restart marks the gap as unknown.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.recordHistory(ctx)
	if latest, _ := srv.db.LatestStatuses(); latest["host/node1"] != "unknown" {
		t.Errorf("after a restart node1 = %q, want unknown", latest["host/node1"])
	}
}

func TestEmptyReportHasNoNullLists(t *testing.T) {
	srv := stateServer(t, t.TempDir(), true)
	w := httptest.NewRecorder()
	srv.handleReports(w, httptest.NewRequest("GET", "/api/v1/reports?period=7d", nil))
	if strings.Contains(strings.Replace(w.Body.String(), `"digest":null`, "", 1), "null") { // no digest configured is a real null
		t.Errorf("a fresh database gave nulls: %s", w.Body)
	}
}

func TestServerZone(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	if z := serverZone(); z != "Europe/Berlin" {
		t.Fatalf("with TZ: %q", z)
	}
	t.Setenv("TZ", "Not/AZone")
	if z := serverZone(); z == "Not/AZone" || z == "" || z == "Local" {
		t.Fatalf("a bad TZ was used: %q", z)
	}
	if _, err := time.LoadLocation(serverZone()); err != nil {
		t.Fatal(err)
	}
}
