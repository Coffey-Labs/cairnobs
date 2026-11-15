package server

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A Windows host's event log: its groups on the host, a warning for a
// critical event in the last hour, none for errors or older criticals.
func TestEvents(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	r := results(now)
	r.OS = "Windows Server 2025 Datacenter Evaluation 24H2"
	r.Events = &report.Events{At: now, Since: now.Add(-24 * time.Hour), List: []report.EventGroup{
		{Log: "System", Source: "Microsoft-Windows-Kernel-Power", ID: 41, Level: "critical", Count: 1, First: now.Add(-10 * time.Minute), Last: now.Add(-10 * time.Minute),
			Message: "The system has rebooted without cleanly shutting down first."},
		{Log: "System", Source: "disk", ID: 11, Level: "critical", Count: 3, First: now.Add(-5 * time.Hour), Last: now.Add(-3 * time.Hour)},
		{Log: "Application", Source: "MSSQLSERVER", ID: 17204, Level: "error", Count: 12, First: now.Add(-time.Hour), Last: now},
	}}
	srv.store.Put("win1", r, now)
	srv.store.Put("win1", results(now.Add(time.Second)), now.Add(time.Second)) // kept between reports

	hosts, conds := srv.view(now)
	var mine []Condition
	for _, c := range conds {
		if strings.HasPrefix(c.Key, "event/") {
			mine = append(mine, c)
		}
	}
	if len(mine) != 1 || mine[0].Key != "event/win1/System/Microsoft-Windows-Kernel-Power/41" || mine[0].Severity != "warning" ||
		mine[0].Title != "win1 logged critical event 41 from Microsoft-Windows-Kernel-Power" ||
		!strings.HasPrefix(mine[0].Detail, "The system has rebooted without cleanly shutting down first. (1 in the last day") {
		t.Errorf("conditions: %+v", mine)
	}
	for _, h := range hosts {
		if h.Name == "win1" && (h.Events == nil || len(h.Events.List) != 3) {
			t.Errorf("host view: %+v", h.Events)
		}
	}
	// Old news: the read is from three hours ago, and is dropped.
	r.Events.At = now.Add(-3 * time.Hour)
	srv.store.Put("win1", r, now)
	hosts, _ = srv.view(now)
	for _, h := range hosts {
		if h.Name == "win1" && h.Events != nil {
			t.Error("kept a stale read")
		}
	}
}
