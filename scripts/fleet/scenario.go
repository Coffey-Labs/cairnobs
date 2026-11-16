package main

import (
	"fmt"
	"hash/fnv"
	"time"
)

// What goes wrong in the demo, and when (#35). A real fleet is mostly
// fine: a few things stay wrong until someone gets to them, and the rest
// come and go. So the demo has
//
//   - three stories, each a cause with its effects:
//     1. fa-prod-01 lost a drive: the array is degraded and its writes are
//     slow while the drive's data is rebuilt (all the time);
//     2. checkout errors: about every 2 hours phx1-db03, a PostgreSQL replica,
//     falls behind, HAProxy takes it out, and the shop's checkout answers
//     with errors until it catches up (20 minutes);
//     3. the legacy datacenter: about every 4 hours its server stops answering
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

// spell is when one problem happens: about once every every, about at
// into each period, for about on. A zero every means all the time.
//
// Not like a clock (#57): each period's start moves by up to an eighth of
// the period, its length is 70% to 130% of on, and half the periods have
// none, four in five at night in Phoenix, where the fleet's main site is.
// That still starts something every 20 minutes or so, while 30 days of
// history hold about 1,400 incidents rather than 3,700. All of it is
// seeded by the spell and the period, so it's the same whenever it's
// worked out.
type spell struct {
	every, at, on time.Duration
}

// always is a problem that's there all the time.
var always = spell{}

// occurrence is one spell, from start to end.
type occurrence struct{ start, end time.Time }

var phoenix = time.FixedZone("MST", -7*3600)

// nth is the spell in period n, counting from the Unix epoch; false when
// that period has none.
func (s spell) nth(n int64) (occurrence, bool) {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d %d %d %d", s.every, s.at, s.on, n)
	x := h.Sum64() // FNV leaves short, alike inputs' high bits alike: mix them
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	frac := func(shift uint) float64 { return float64((x>>shift)&0xffff) / 0x10000 }
	on := time.Duration(float64(s.on) * (0.7 + 0.6*frac(0)))
	at := s.at + time.Duration((2*frac(16)-1)*float64(s.every/8))
	at = max(0, min(at, s.every-on)) // inside its own period
	start := time.Unix(0, n*int64(s.every)).Add(at)
	skip := 0.5
	if hr := start.In(phoenix).Hour(); hr < 7 || hr >= 22 {
		skip = 0.8
	}
	if frac(32) < skip {
		return occurrence{}, false
	}
	return occurrence{start, start.Add(on)}, true
}

func (s spell) period(t time.Time) int64 { return t.UnixNano() / int64(s.every) }

func (s spell) active(t time.Time) bool {
	if s.every == 0 {
		return true
	}
	o, ok := s.nth(s.period(t))
	return ok && !t.Before(o.start) && t.Before(o.end)
}

// last is the spell going on at t, or the last one before it; for a
// problem that's always there, one that began long ago and hasn't ended.
func (s spell) last(t time.Time) occurrence {
	if s.every == 0 {
		return occurrence{t.Add(-9 * 24 * time.Hour), t.Add(100 * 365 * 24 * time.Hour)}
	}
	for n := s.period(t); ; n-- {
		if o, ok := s.nth(n); ok && !o.start.After(t) {
			return o
		}
	}
}

// began is when the current spell started, or the last one if it's over.
func (s spell) began(t time.Time) time.Time { return s.last(t).start }

// between is every spell that starts from from to to, oldest first.
func (s spell) between(from, to time.Time) []occurrence {
	if s.every == 0 {
		return nil
	}
	var out []occurrence
	for n := s.period(from); n <= s.period(to); n++ {
		if o, ok := s.nth(n); ok && !o.start.Before(from) && !o.start.After(to) {
			out = append(out, o)
		}
	}
	return out
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

// past is a problem as obs-beacond titles it, for the incidents the demo's
// history starts with (history.go): one for each time its spell came and
// went before the demo started. The keys and titles are obs-beacond's
// own, so the past and the present read alike.
type past struct {
	s                     spell
	key, sev, title, host string
	href, detail, note    string // note: what someone wrote afterwards, on the first of the stories' incidents in a day
}

var pastProblems = []past{
	{s: spellLegacyDC, key: "akamai/property/legacy.corp.example.akadns.net", sev: "critical", href: "/akamai",
		title:  "Akamai GTM: legacy.corp.example.akadns.net has no live datacenter",
		detail: "Every datacenter's server is failing the liveness test, so GTM answers with all of them, or none.",
		note:   "Ashburn lost its upstream link for half an hour. The legacy app has one datacenter, so GTM had nowhere else to send it."},
	{s: spellLegacyDC, key: "akamai/traffic/400019", sev: "critical", href: "/akamai",
		title:  "Akamai: 30% of CP code 400019's requests are failing with 5xx",
		detail: "Edge errors for the legacy property: its origin is the Ashburn server GTM can't reach."},
	{s: spellLegacyDC, key: "akamai/server/legacy.corp.example.akadns.net/198.51.100.50", sev: "warning", href: "/akamai",
		title: "198.51.100.50 in Ashburn is failing GTM's liveness test for legacy.corp.example.akadns.net"},
	{s: spellReplicaBehind, key: "cloudflare/errors/shop.example", sev: "warning", href: "/cloudflare",
		title:  "shop.example: 7% of requests failing",
		detail: "Most on checkout.shop.example: 503s while HAProxy had one PostgreSQL replica for the shop's reads.",
		note:   hostName(pgLagging) + " fell 40 MB behind during the nightly vacuum and HAProxy took it out; the one replica left couldn't carry checkout's reads. Moving the vacuum out of business hours."},
	{s: spellAppExits, key: "container/" + hostName(demoExited) + "/app-04", sev: "critical", host: hostName(demoExited), href: "/containers",
		title: "app-04 on " + hostName(demoExited) + ": not running"},
	{s: spellDRPaths, key: "cluster/" + storageCollector.name + ":fa-dr-01/connection/fa-prod-01", sev: "critical", host: storageCollector.name + ":fa-dr-01", href: "/clusters",
		title: "Replication to fa-prod-01 in fa-dr-01 (" + storageCollector.name + "): connected, but its paths aren't redundant"},
	{s: spellLBFlaps, key: "cloudflare/origin/site-1-web/" + hostName(52), sev: "warning", host: hostName(52), href: "/cloudflare",
		title: hostName(52) + " is failing site-1-web's health check"},
	{s: spellLBFlaps, key: "akamai/server/api.shop.example.akadns.net/203.0.2.11", sev: "warning", host: hostName(101), href: "/akamai",
		title: "203.0.2.11 in Singapore is failing GTM's liveness test for api.shop.example.akadns.net"},
	{s: spellVaultEdgeDown, key: "node/obs-beacond:vault-edge/vault-edge-e3", sev: "critical", host: "obs-beacond:vault-edge",
		href: "/workloads?source=obs-beacond%3Avault-edge&q=vault-edge-e3", title: "Server vault-edge-e3 in vault-edge (obs-beacond) is down"},
	{s: spellCrashloop, key: "workload/cluster-00/ns-07/deployment/app-097", sev: "warning", host: "cluster-00",
		href: "/workloads?source=cluster-00&ns=ns-07&q=app-097", title: "deployment/app-097 in ns-07 (cluster-00): 1 of 10 pods: 1 restarting (CrashLoopBackOff)"},
	{s: spellLegacy502, key: "check/web-legacy", sev: "warning", href: "/checks", title: "legacy.example.com is down"},
	{s: spellBlocklisted, key: "check/rbl-mx2", sev: "warning", href: "/checks", title: "mx2.example.com on block lists is down"},
	{s: spellLateBackup, key: "backup/" + hostName(50) + "/received/" + hostName(80) + "/pg-dump", sev: "serious", host: hostName(50), href: "/backups",
		title: hostName(80) + "'s pg-dump backup on " + hostName(50) + " is 26 hours old"},
}
