package main

import "time"

// What goes wrong in the demo, and when (#35). A real fleet is mostly
// fine: a few things stay wrong until someone gets to them, and the rest
// come and go. So the demo has
//
//   - three stories, each a cause with its effects:
//     1. fa-prod-01 lost a drive: the array is degraded and its writes are
//     slow while the drive's data is rebuilt (all the time);
//     2. checkout errors: every 2 hours fleet-0032, a PostgreSQL replica,
//     falls behind, HAProxy takes it out, and the shop's checkout answers
//     with errors until it catches up (20 minutes);
//     3. the legacy datacenter: every 4 hours its server stops answering
//     Akamai's liveness test, GTM has nowhere to send the legacy property,
//     and its CP code fails (30 minutes);
//   - a few lasting warnings anyone would have: a failover nobody moved
//     back, a weak key, a VM snapshot left behind, a datastore filling, a
//     zone on Flexible SSL, a renewal waiting on someone;
//   - problems that come and go: a unit that fails and is restarted, a
//     container that exits, a check that flaps, a host update that takes
//     a while, a late backup, an IBM i job waiting on an answer.
//
// Spells count from the Unix epoch, not from when the simulator started,
// so every part of it agrees on what's happening now, a restarted demo
// picks up where the clock is, and the history the demo starts with can
// be worked out for any time in the past.

// spell is when one problem is happening: for on, once every every, at
// into each period. A zero every means all the time.
type spell struct {
	every, at, on time.Duration
}

// always is a problem that's there all the time.
var always = spell{}

func (s spell) active(t time.Time) bool {
	if s.every == 0 {
		return true
	}
	p := time.Duration(t.UnixNano()) % s.every
	return p >= s.at && p < s.at+s.on
}

// began is when the current spell started, or the last one if it's over;
// a long time ago for a problem that's always there.
func (s spell) began(t time.Time) time.Time {
	if s.every == 0 {
		return t.Add(-9 * 24 * time.Hour)
	}
	p := time.Duration(t.UnixNano()) % s.every
	start := t.Add(-p + s.at)
	if start.After(t) {
		start = start.Add(-s.every)
	}
	return start
}

// The spells, by story. Periods aren't multiples of each other, so the
// mix on screen changes through the day.
var (
	// Story 2: checkout errors.
	spellReplicaBehind = spell{every: 2 * time.Hour, at: 40 * time.Minute, on: 20 * time.Minute}
	// Story 3: the legacy datacenter.
	spellLegacyDC = spell{every: 4 * time.Hour, at: 70 * time.Minute, on: 30 * time.Minute}

	// Coming and going.
	spellRedisFails    = spell{every: 110 * time.Minute, at: 20 * time.Minute, on: 12 * time.Minute}
	spellAppExits      = spell{every: 75 * time.Minute, at: 5 * time.Minute, on: 8 * time.Minute}
	spellLegacy502     = spell{every: 140 * time.Minute, at: 30 * time.Minute, on: 15 * time.Minute}
	spellLBFlaps       = spell{every: 100 * time.Minute, at: 60 * time.Minute, on: 15 * time.Minute}
	spellCrashloop     = spell{every: 150 * time.Minute, at: 15 * time.Minute, on: 20 * time.Minute}
	spellESXiUpdate    = spell{every: 4 * time.Hour, at: 10 * time.Minute, on: 40 * time.Minute}
	spellESXiMemory    = spell{every: 130 * time.Minute, at: 50 * time.Minute, on: 15 * time.Minute}
	spellAHVUpdate     = spell{every: 270 * time.Minute, at: 100 * time.Minute, on: 40 * time.Minute}
	spellPVEHA         = spell{every: 150 * time.Minute, at: 90 * time.Minute, on: 20 * time.Minute}
	spellDRPaths       = spell{every: 3 * time.Hour, at: 130 * time.Minute, on: 30 * time.Minute}
	spellBladeEvac     = spell{every: 6 * time.Hour, at: 2 * time.Hour, on: 60 * time.Minute}
	spellVDIUpdate     = spell{every: 5 * time.Hour, at: 4 * time.Hour, on: 45 * time.Minute}
	spellInquiry       = spell{every: 3 * time.Hour, at: 110 * time.Minute, on: 30 * time.Minute}
	spellOrdersDown    = spell{every: 200 * time.Minute, at: 80 * time.Minute, on: 25 * time.Minute}
	spellVaultEdgeDown = spell{every: 3 * time.Hour, at: 150 * time.Minute, on: 20 * time.Minute}
	spellBlocklisted   = spell{every: 5 * time.Hour, at: 3 * time.Hour, on: 40 * time.Minute}
	spellLateBackup    = spell{every: 6 * time.Hour, at: 5 * time.Hour, on: 50 * time.Minute}
	spellBackupShrank  = spell{every: 6 * time.Hour, at: 2 * time.Hour, on: 50 * time.Minute}
	spellSQLAgent      = spell{every: 4 * time.Hour, at: 3 * time.Hour, on: 45 * time.Minute}
	spellFSCrash       = spell{every: 8 * time.Hour, at: 7 * time.Hour, on: time.Hour}
	spellActivation    = spell{every: 3 * time.Hour, at: 20 * time.Minute, on: 80 * time.Minute}
)
