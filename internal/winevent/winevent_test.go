package winevent

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// What Windows renders for Kernel-Power 41 (trimmed).
const kernelPower = `<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System><Provider Name='Microsoft-Windows-Kernel-Power' Guid='{331c3b3a-2005-44c2-ac5e-77220c37d6b4}'/><EventID>41</EventID><Version>8</Version><Level>1</Level><Task>63</Task><TimeCreated SystemTime='2026-11-15T08:12:03.5141390Z'/><Channel>System</Channel><Computer>win1</Computer></System><EventData><Data Name='BugcheckCode'>0</Data></EventData></Event>`

func TestParseXML(t *testing.T) {
	e, ok := ParseXML("System", []byte(kernelPower))
	if !ok || e.Source != "Microsoft-Windows-Kernel-Power" || e.ID != 41 || e.Level != 1 || e.Log != "System" ||
		!e.Time.Equal(time.Date(2026, 11, 15, 8, 12, 3, 514139000, time.UTC)) {
		t.Fatalf("%+v %v", e, ok)
	}
	if _, ok := ParseXML("System", []byte("<Event><System><EventID>x</EventID></System></Event>")); ok {
		t.Error("a bad ID parsed")
	}
}

func TestGroup(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	ev := []Event{ // newest first, as read
		{Log: "Application", Source: "MSSQLSERVER", ID: 17204, Level: 2, Time: at, Message: "FCB::Open failed:\r\n  Could not open file  D:\\data\\x.mdf"},
		{Log: "System", Source: "disk", ID: 7, Level: 2, Time: at.Add(-time.Minute), Message: "The device, \\Device\\Harddisk1\\DR1, has a bad block."},
		{Log: "Application", Source: "MSSQLSERVER", ID: 17204, Level: 2, Time: at.Add(-time.Hour), Message: "older"},
		{Log: "System", Source: "Microsoft-Windows-Kernel-Power", ID: 41, Level: 1, Time: at.Add(-2 * time.Hour)},
	}
	g := Group(ev)
	if len(g) != 3 {
		t.Fatalf("%d groups: %+v", len(g), g)
	}
	if s := g[0]; s.Source != "MSSQLSERVER" || s.Count != 2 || s.Level != "error" || !s.Last.Equal(at) || !s.First.Equal(at.Add(-time.Hour)) ||
		s.Message != `FCB::Open failed: Could not open file D:\data\x.mdf` {
		t.Errorf("SQL Server's: %+v", s)
	}
	if k := g[2]; k.Source != "Microsoft-Windows-Kernel-Power" || k.Level != "critical" || k.Count != 1 {
		t.Errorf("Kernel-Power's: %+v", k)
	}
	if m := Shorten(strings.Repeat("é", 200)); len(m) > maxMessage+len("…") || !strings.HasSuffix(m, "…") {
		t.Errorf("shortened to %d bytes", len(m))
	}
}

func TestWatch(t *testing.T) {
	var w Watch
	now := time.Now()
	if w.Report(now) != nil {
		t.Error("a report before the first read")
	}
	e := &report.Events{List: []report.EventGroup{{Log: "System", Source: "disk", ID: 7, Count: 1, Last: now}}}
	w.Set(e)
	if r := w.Report(now); r == nil || len(r.List) != 1 || r.At.IsZero() {
		t.Fatalf("first: %+v", r)
	}
	w.Set(e)
	if w.Report(now.Add(time.Minute)) != nil {
		t.Error("sent unchanged")
	}
	if w.Report(now.Add(2*time.Hour)) == nil {
		t.Error("not sent again after an hour")
	}
	w.Set(&report.Events{List: []report.EventGroup{{Log: "System", Source: "disk", ID: 7, Count: 2, Last: now}}})
	if w.Report(now.Add(2*time.Hour+time.Minute)) == nil {
		t.Error("a new count not sent")
	}
}
