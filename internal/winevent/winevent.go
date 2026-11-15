// Package winevent reads a Windows host's event log for the agent
// (docs/windows.md, step 7): the errors and criticals in System and
// Application over the last day, grouped by log, source and event ID, so
// a host's page says what Windows has been complaining about. The
// grouping is plain Go; the reading is Windows' (read_windows.go).
package winevent

import (
	"cmp"
	"encoding/xml"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cairnobs/cairnobs/internal/report"
)

const (
	// Window is how far back each read looks.
	Window = 24 * time.Hour
	// Every is how often the agent reads.
	Every = 5 * time.Minute
	// PerLog caps the events read from one log in one read, newest first.
	PerLog = 2000
	// resend is how often an unchanged list goes again, so a restarted
	// obs-beacond has it within the hour.
	resend     = time.Hour
	maxMessage = 300
	maxGroups  = 100
)

// Logs are the logs read.
var Logs = []string{"System", "Application"}

// Event is one event, as read.
type Event struct {
	Log     string
	Source  string
	ID      uint32
	Level   int // 1 critical, 2 error
	Time    time.Time
	Message string
}

// Group is the events grouped by log, source and ID, most recent first, at
// most 100 groups; each keeps its latest message.
func Group(events []Event) []report.EventGroup {
	type key struct {
		log, source string
		id          uint32
	}
	by := map[key]*report.EventGroup{}
	for _, e := range events {
		k := key{e.Log, e.Source, e.ID}
		g := by[k]
		if g == nil {
			g = &report.EventGroup{Log: e.Log, Source: e.Source, ID: e.ID, First: e.Time, Last: e.Time}
			by[k] = g
		}
		g.Count++
		if e.Level == 1 {
			g.Level = "critical"
		} else if g.Level == "" {
			g.Level = "error"
		}
		if e.Time.Before(g.First) {
			g.First = e.Time
		}
		if !e.Time.Before(g.Last) || g.Message == "" {
			if !e.Time.Before(g.Last) {
				g.Last = e.Time
			}
			if m := Shorten(e.Message); m != "" {
				g.Message = m
			}
		}
	}
	out := make([]report.EventGroup, 0, len(by))
	for _, g := range by {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b report.EventGroup) int {
		return cmp.Or(b.Last.Compare(a.Last), cmp.Compare(a.Log, b.Log), cmp.Compare(a.Source, b.Source), cmp.Compare(a.ID, b.ID))
	})
	if len(out) > maxGroups {
		out = out[:maxGroups]
	}
	return out
}

// Shorten is a message on one line, at most 300 bytes.
func Shorten(m string) string {
	m = strings.Join(strings.Fields(m), " ")
	if len(m) <= maxMessage {
		return m
	}
	m = m[:maxMessage]
	for !utf8.ValidString(m) {
		m = m[:len(m)-1]
	}
	return m + "…"
}

// systemXML is the part of an event's XML the agent reads.
type systemXML struct {
	System struct {
		Provider struct {
			Name string `xml:"Name,attr"`
		} `xml:"Provider"`
		EventID     string `xml:"EventID"`
		Level       int    `xml:"Level"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
		Channel string `xml:"Channel"`
	} `xml:"System"`
}

// ParseXML is an event from its XML, as Windows renders it.
func ParseXML(log string, b []byte) (Event, bool) {
	var x systemXML
	if err := xml.Unmarshal(b, &x); err != nil {
		return Event{}, false
	}
	id, err := strconv.ParseUint(strings.TrimSpace(x.System.EventID), 10, 32)
	if err != nil {
		return Event{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, x.System.TimeCreated.SystemTime)
	if err != nil {
		return Event{}, false
	}
	return Event{Log: log, Source: x.System.Provider.Name, ID: uint32(id), Level: x.System.Level, Time: t.UTC()}, true
}

// Watch keeps the latest read for the reports.
type Watch struct {
	mu   sync.Mutex
	cur  *report.Events
	last string
	sent time.Time
}

// Set keeps a read.
func (w *Watch) Set(e *report.Events) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur = e
}

// Report is the events to send: when they changed, or hourly; nil
// otherwise (obs-beacond keeps the last).
func (w *Watch) Report(now time.Time) *report.Events {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	var key strings.Builder
	key.WriteString(w.cur.Error)
	for _, g := range w.cur.List {
		key.WriteString(g.Log + "|" + g.Source + "|" + strconv.Itoa(int(g.ID)) + "|" + strconv.Itoa(g.Count) + "|" + g.Last.String() + ";")
	}
	if key.String() == w.last && now.Sub(w.sent) < resend {
		return nil
	}
	w.last, w.sent = key.String(), now
	e := *w.cur
	e.At = now.UTC()
	e.List = slices.Clone(e.List)
	return &e
}
