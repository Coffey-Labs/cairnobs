package main

import (
	"hash/fnv"
	"math"
	"time"
)

// What a fake host uses, worked out from its name and the clock alone, so
// the live reports and the 30 days written before them (history.go)
// agree: a working day in its site's time zone and quieter weekends,
// memory that creeps on some hosts until a weekly restart, and disks that
// fill slowly until someone cleans up.

// seed is 0 … 1, fixed for a host and what it's for.
func seed(name, what string) float64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(what))
	return float64(h.Sum64()%1_000_003) / 1_000_003
}

// jitter is -1 … 1, fixed for a host and a moment (to the second), so a
// reading looks like a reading and the same moment always reads the same.
func jitter(name string, t time.Time) float64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	var b [8]byte
	u := uint64(t.Unix())
	for i := range b {
		b[i] = byte(u >> (8 * i))
	}
	h.Write(b[:])
	return float64(h.Sum64()%2_000_001)/1_000_000 - 1
}

// siteZone is the hours from UTC where a host's site is: the first site
// in Phoenix, then the US east coast, London and Frankfurt in turn.
func siteZone(name string) time.Duration {
	n := fleetIndex(-1, name)
	if n < 0 {
		return -7 * time.Hour // the Windows servers are at the first site
	}
	return []time.Duration{-7 * time.Hour, -5 * time.Hour, 0, time.Hour}[(n/siteSize)%4]
}

// workday is 0 … 1: how busy the working day is at t, where the host is.
// It peaks early afternoon, is near nothing at night, and a weekend is a
// third of a weekday.
func workday(name string, t time.Time) float64 {
	local := t.Add(siteZone(name)).UTC()
	h := float64(local.Hour()) + float64(local.Minute())/60
	d := 0.0
	if h > 7 && h < 20 {
		d = math.Sin(math.Pi * (h - 7) / 13)
	}
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		d /= 3
	}
	return d
}

// cpuAt is a host's CPU use (percent) at t, without the moment's jitter.
// A third of hosts run a nightly job at 02:00 for half an hour.
func cpuAt(name string, t time.Time) float64 {
	base := 3 + 17*seed(name, "cpu")
	swing := base * (0.5 + 1.5*seed(name, "cpu swing"))
	cpu := base + swing*workday(name, t)
	if seed(name, "nightly") < 0.33 {
		local := t.Add(siteZone(name)).UTC()
		if local.Hour() == 2 && local.Minute() < 30 {
			cpu += 25 + 30*seed(name, "nightly size")
		}
	}
	return min(cpu, 97)
}

// cpuNow is cpuAt with the moment's jitter, as one reading.
func cpuNow(name string, t time.Time) float64 {
	return max(0.5, cpuAt(name, t)*(1+0.25*jitter(name, t)))
}

// memAt is a host's memory use (percent) at t. Two hosts in five run
// something that grows until it's restarted, once a week.
func memAt(name string, t time.Time) float64 {
	m := 30 + 40*seed(name, "mem") + 5*workday(name, t)
	if seed(name, "leaks") < 0.4 {
		week := 7 * 24 * time.Hour
		phase := time.Duration(seed(name, "leak phase") * float64(week))
		frac := float64((t.Add(phase).UnixNano())%int64(week)) / float64(week)
		m += 14 * frac
	}
	return min(m, 88) // memory warns at 90
}

// diskAt is how full (0 … 1) a host's disk k is at t: a level of its own,
// growing slowly over a cycle of one to three months until a clean-up
// brings it back. phx1-app20's log volume is the one that fills: from 58%
// to 86% in ten days, so it warns now and then.
func diskAt(name string, k int, t time.Time) float64 {
	what := []string{"root", "data"}[min(k, 1)]
	base := 0.12 + 0.48*seed(name, what)
	grow := 0.04 + 0.12*seed(name, what+" growth")
	cycle := time.Duration(30+60*seed(name, what+" cycle")) * 24 * time.Hour
	if name == hostName(23) && k == 1 {
		base, grow, cycle = 0.58, 0.28, 10*24*time.Hour
	}
	phase := time.Duration(seed(name, what+" phase") * float64(cycle))
	frac := float64(t.Add(phase).UnixNano()%int64(cycle)) / float64(cycle)
	return min(base+grow*frac, 0.95)
}
