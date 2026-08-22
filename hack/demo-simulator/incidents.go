package main

import (
	"math"
	"time"
)

// Incidents are what turn a wall of uniform noise into a dataset worth
// clicking around in: a demo viewer who filters to 5xx, or opens the
// Alerts page, should find a *story* (one bad API node, a burst of SSH
// probing, a spam wave) rather than the same flat error rate everywhere.
// Every window below is expressed relative to `origin` -- the moment the
// simulator started, which is also the end of the backfill window -- so
// a freshly reset demo always has its incidents at the same, recent,
// predictable offsets no matter what day it is.
type conditions struct {
	// apiErrorRate replaces the API tier's baseline 5xx probability for
	// the affected host during an outage window.
	apiErrorRate float64
	// latencyMult multiplies API/DB latencies -- an outage that only
	// changed status codes without slowing anything down wouldn't look
	// like a real one.
	latencyMult float64
	// jobFailureRate replaces the worker tier's baseline failure
	// probability. An API/database outage doesn't stay in the request
	// path: the same failing dependencies take background jobs down with
	// them, which is also the only thing that ever makes the job-failure
	// alert rule true -- a steady 5% background failure rate is normal
	// operation, not an incident.
	jobFailureRate float64
	// bruteForce and spamWave switch the system/smtp generators from
	// their normal mix to an attack-shaped one for the window.
	bruteForce bool
	spamWave   bool
}

// Windows, as offsets back from origin. Kept as one table so the story
// is readable in one place and the alert rules in
// /hack/demo-seed/alerts can be written against known-true conditions.
const (
	apiOutageStart = 8 * time.Hour
	apiOutageEnd   = 6*time.Hour + 30*time.Minute
	apiOutageHost  = "api-02"

	bruteForceStart = 14 * time.Hour
	bruteForceEnd   = 13 * time.Hour

	spamWaveStart = 30 * time.Hour
	spamWaveEnd   = 26 * time.Hour

	// Live mode can't rely on the backfill windows above -- they recede
	// into the past as a demo session runs. These recurring bursts keep
	// the *present* interesting too, which is what the alert rules
	// (evaluating over -5m/-10m windows) actually see: without them,
	// every rule would settle into a permanent OK state a few minutes
	// after a reset and the Alerts page would never do anything again.
	liveErrorBurstPeriod = 47 * time.Minute
	liveErrorBurstLen    = 5 * time.Minute
	liveProbePeriod      = 2 * time.Hour
	liveProbeLen         = 8 * time.Minute
	liveSpamPeriod       = 3 * time.Hour
	liveSpamLen          = 10 * time.Minute
)

func conditionsAt(t, origin time.Time, h *host) conditions {
	hostName := h.name
	c := conditions{latencyMult: 1}
	since := origin.Sub(t)

	// Is an API-tier outage in effect? Two sources feed the same
	// handling: the backfilled incident window, and -- in live mode --
	// the recurring burst that keeps the present interesting.
	outage := 0.0
	if since <= apiOutageStart && since >= apiOutageEnd {
		outage = 0.42
	}
	bruteForce := since <= bruteForceStart && since >= bruteForceEnd
	spamWave := since <= spamWaveStart && since >= spamWaveEnd

	if t.After(origin) {
		// Phases are measured from origin so the first burst of each kind
		// lands a predictable few minutes into a demo session rather than
		// immediately at reset.
		elapsed := t.Sub(origin)
		if phase := (elapsed + 10*time.Minute) % liveErrorBurstPeriod; phase < liveErrorBurstLen {
			outage = 0.45
		}
		if phase := (elapsed + 20*time.Minute) % liveProbePeriod; phase < liveProbeLen {
			bruteForce = true
		}
		if phase := (elapsed + 35*time.Minute) % liveSpamPeriod; phase < liveSpamLen {
			spamWave = true
		}
	}

	if outage > 0 {
		switch {
		case hostName == apiOutageHost:
			c.apiErrorRate = outage
			c.latencyMult = 4.5
		case internetFacingName(hostName):
			// The edge tier fronts all three API nodes, so roughly a
			// third of what it proxies during the outage hits the failing
			// one. Without this the outage would be invisible from the
			// edge, which isn't how a viewer expects to be able to trace
			// it: client-visible 5xx are exactly what makes it an outage
			// rather than an internal blip.
			c.apiErrorRate = outage / 3
			c.latencyMult = 2
		case hostName == "db-01":
			// The database is the *cause*, not a second unrelated
			// incident -- a viewer who drills from the API errors into
			// the same window on db-01 should find slow queries waiting.
			c.latencyMult = 6
		case h.service == "worker":
			c.jobFailureRate = 0.35
			c.latencyMult = 2.5
		}
	}
	c.bruteForce = bruteForce
	c.spamWave = spamWave

	return c
}

// diurnal scales event rates by time of day: a shop's traffic peaks
// mid-afternoon UTC and bottoms out around 04:00, roughly a 3.5x spread.
// Without this every chart is a flat line and the "last 24h" view tells
// a viewer nothing that "last 1h" didn't.
func diurnal(t time.Time) float64 {
	hour := float64(t.UTC().Hour()) + float64(t.UTC().Minute())/60
	// Peak at 15:00 UTC, trough at 03:00.
	return 0.35 + 0.65*(0.5+0.5*math.Cos((hour-15)/24*2*math.Pi))
}
