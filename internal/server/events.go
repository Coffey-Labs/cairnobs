package server

import (
	"fmt"
	"strconv"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// eventRecent is how long a critical event in a Windows host's event log
// keeps its warning (docs/windows.md, step 7).
const eventRecent = time.Hour

// eventConditions are a Windows host's critical events of the last hour,
// one warning per log, source and event ID: an unexpected restart
// (Kernel-Power 41), a disk or file system failing, a service crashing
// the way Windows calls critical. Errors are only listed on the host's
// page: Windows logs plenty that nobody needs woken for.
func eventConditions(host string, e *report.Events, now time.Time) []Condition {
	var out []Condition
	for _, g := range e.List {
		if g.Level != "critical" || now.Sub(g.Last) > eventRecent {
			continue
		}
		detail := fmt.Sprintf("%d in the last day, the latest at %s UTC.", g.Count, g.Last.UTC().Format("15:04"))
		if g.Message != "" {
			detail = g.Message + " (" + detail + ")"
		}
		out = append(out, Condition{
			Key:      "event/" + host + "/" + g.Log + "/" + g.Source + "/" + strconv.FormatUint(uint64(g.ID), 10),
			Severity: "warning",
			Title:    fmt.Sprintf("%s logged critical event %d from %s", host, g.ID, g.Source),
			Detail:   detail,
			Href:     "/hosts/" + host + "#events",
		})
	}
	return out
}
