package units

import (
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Windows services are told to obs-beacond as units: the server names
// them as it names systemd's ("W32Time" in expected.yaml is asked for as
// W32Time.service) and judges them the same way (docs/windows.md).

// SCM service states and start types, as winsvc.h numbers them.
const (
	scmStopped         = 1
	scmStartPending    = 2
	scmStopPending     = 3
	scmRunning         = 4
	scmContinuePending = 5
	scmPausePending    = 6
	scmPaused          = 7

	scmAutoStart     = 2
	scmDisabledStart = 4
)

// ServiceName is the Windows service a unit name means.
func ServiceName(unit string) string { return strings.TrimSuffix(unit, ".service") }

// scmUnit says what systemd would of a service the SCM describes. One that
// should start by itself, or that stopped with an error, has failed;
// stopped otherwise, it's inactive.
func scmUnit(unit string, state, startType, exitCode uint32, since time.Time) report.Unit {
	u := report.Unit{Name: unit, Load: "loaded", Since: since}
	switch state {
	case scmRunning:
		u.Active, u.Sub = "active", "running"
	case scmPaused, scmPausePending, scmContinuePending:
		u.Active, u.Sub = "active", "paused"
	case scmStartPending:
		u.Active, u.Sub = "activating", "start"
	case scmStopPending:
		u.Active, u.Sub = "deactivating", "stop"
	default:
		u.Active, u.Sub, u.Since = "inactive", "dead", time.Time{}
		if startType == scmAutoStart || exitCode != 0 {
			u.Active = "failed"
		}
		if startType == scmDisabledStart {
			u.Sub = "disabled"
		}
	}
	return u
}

// notFound is a service that isn't there.
func notFound(unit string) report.Unit {
	return report.Unit{Name: unit, Load: "not-found", Active: "inactive", Sub: "dead"}
}
