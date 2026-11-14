// Package maint is maintenance windows: daily times when a host is
// expected to go away (the nightly auto-reboot) and nobody should be woken.
package maint

import (
	"fmt"
	"time"
	_ "time/tzdata" // obs-beacond and the agent run without the system's zone files
)

// Window is one daily window, in its own timezone (so it follows daylight
// saving the way the host's own timer does).
type Window struct {
	Name     string        `yaml:"name" json:"name"`
	Hosts    []string      `yaml:"hosts" json:"hosts,omitempty"` // obs-beacond: whose alerts are held
	Daily    string        `yaml:"daily" json:"daily"`           // HH:MM
	Timezone string        `yaml:"timezone" json:"timezone"`     // e.g. Europe/Berlin; default UTC
	Duration time.Duration `yaml:"duration" json:"duration_s"`   // how long it stays open

	loc  *time.Location
	h, m int
}

// Check parses the window and refuses one that can't work.
func (w *Window) Check() error {
	if w.Timezone == "" {
		w.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return fmt.Errorf("maintenance window %q: %w", w.Name, err)
	}
	w.loc = loc
	if _, err := fmt.Sscanf(w.Daily, "%d:%d", &w.h, &w.m); err != nil || w.h < 0 || w.h > 23 || w.m < 0 || w.m > 59 || len(w.Daily) != 5 {
		return fmt.Errorf("maintenance window %q: daily must be HH:MM", w.Name)
	}
	if w.Duration <= 0 || w.Duration > 12*time.Hour {
		return fmt.Errorf("maintenance window %q: duration must be between 1s and 12h", w.Name)
	}
	return nil
}

// start is the window's start on the day of t (in its timezone).
func (w *Window) start(t time.Time) time.Time {
	t = t.In(w.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), w.h, w.m, 0, 0, w.loc)
}

// Active says whether now falls in the window (including one that began
// yesterday and runs past midnight).
func (w *Window) Active(now time.Time) bool {
	for _, s := range []time.Time{w.start(now), w.start(now.AddDate(0, 0, -1))} {
		if !now.Before(s) && now.Before(s.Add(w.Duration)) {
			return true
		}
	}
	return false
}

// Next is when the window next opens (now, if it is open).
func (w *Window) Next(now time.Time) time.Time {
	if w.Active(now) {
		return now
	}
	s := w.start(now)
	if !s.After(now) {
		s = w.start(now.AddDate(0, 0, 1))
	}
	return s
}

// AnyActive says whether any of the windows is open.
func AnyActive(ws []Window, now time.Time) bool {
	for i := range ws {
		if ws[i].Active(now) {
			return true
		}
	}
	return false
}
