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
		note:   "fleet-0032 fell 40 MB behind during the nightly vacuum and HAProxy took it out; the one replica left couldn't carry checkout's reads. Moving the vacuum out of business hours."},
	{s: spellAppExits, key: "container/fleet-0007/app-04", sev: "critical", host: "fleet-0007", href: "/containers",
		title: "app-04 on fleet-0007: not running"},
	{s: spellDRPaths, key: "cluster/fleet-0040:fa-dr-01/connection/fa-prod-01", sev: "critical", host: "fleet-0040:fa-dr-01", href: "/clusters",
		title: "Replication to fa-prod-01 in fa-dr-01 (fleet-0040): connected, but its paths aren't redundant"},
	{s: spellLBFlaps, key: "cloudflare/origin/site-1-web/fleet-0052", sev: "warning", host: "fleet-0052", href: "/cloudflare",
		title: "fleet-0052 is failing site-1-web's health check"},
	{s: spellLBFlaps, key: "akamai/server/api.shop.example.akadns.net/203.0.2.11", sev: "warning", host: "fleet-0101", href: "/akamai",
		title: "203.0.2.11 in Singapore is failing GTM's liveness test for api.shop.example.akadns.net"},
	{s: spellVaultEdgeDown, key: "node/obs-beacond:vault-edge/vault-edge-e3", sev: "critical", host: "obs-beacond:vault-edge",
		href: "/workloads?source=obs-beacond%3Avault-edge&q=vault-edge-e3", title: "Server vault-edge-e3 in vault-edge (obs-beacond) is down"},
	{s: spellCrashloop, key: "workload/cluster-00/ns-07/deployment/app-097", sev: "warning", host: "cluster-00",
		href: "/workloads?source=cluster-00&ns=ns-07&q=app-097", title: "deployment/app-097 in ns-07 (cluster-00): 1 of 10 pods: 1 restarting (CrashLoopBackOff)"},
	{s: spellLegacy502, key: "check/web-legacy", sev: "warning", href: "/checks", title: "legacy.example.com is down"},
	{s: spellBlocklisted, key: "check/rbl-mx2", sev: "warning", href: "/checks", title: "mx2.example.com on block lists is down"},
	{s: spellLateBackup, key: "backup/fleet-0050/received/fleet-0080/pg-dump", sev: "serious", host: "fleet-0050", href: "/backups",
		title: "fleet-0080's pg-dump backup on fleet-0050 is 26 hours old"},
}
