package main

// The synthetic fleet the demo deployment pretends to be monitoring: a
// small e-commerce shop's infrastructure. Every host here is fictional,
// but the shape is deliberately realistic -- an edge/nginx tier, a
// three-node API tier, background workers, one Postgres, one Redis, one
// Stalwart mail host, two Windows boxes, and one decommissioned host
// left behind on purpose so the Agents page has a genuinely stale row to
// show (see stale below).
//
// One entry here is one *host*, not one agent process: the `agents`
// table is UNIQUE (tenant_id, host) (see
// metadata/migrations/0037_create_agents.sql), so a host maps to exactly
// one agent row, one metrics series, and one heartbeat stream. Log
// records are not bound by that -- a host emits its primary service's
// logs plus, on Linux, the journald `system` stream every real
// deployment also collects.

type host struct {
	name    string
	service string // primary log service, and the service its agent reports

	// Static context the Hosts page shows alongside utilization (see
	// web/src/lib/api.ts's HostMetrics) -- a viewer can't judge "21% CPU"
	// without the core count, or "is this normal" without uptime.
	os       string
	kernel   string
	arch     string
	cores    int
	memTotal int64
	diskTot  int64
	ipv4     string
	ipv6     string

	// Utilization baselines. Each sample wanders around these rather
	// than being redrawn independently, so the Hosts page shows a host
	// with a personality (a busy API node, an idle cache) instead of the
	// same noise everywhere.
	cpuBase  float64 // mean CPU percent
	memFrac  float64 // mean fraction of memTotal in use
	diskFrac float64 // fraction of diskTot in use at the START of the backfill window
	// diskGrowthPerDay pushes diskFrac up over the window -- the "disk
	// slowly filling up" story the disk-usage alert rule fires on. Zero
	// for every host that isn't part of that story.
	diskGrowthPerDay float64

	// Peak-hour log rates, in events per minute, before the diurnal
	// curve and -rate-scale are applied. systemPerMin is the journald
	// system stream (sshd/ufw/systemd/kernel), zero on Windows hosts.
	eventsPerMin float64
	systemPerMin float64

	// What this host's agent reports about itself on CheckIn.
	agentVersion string
	sourceKind   string // "journald", "file", "eventlog"
	sourceDetail string
	batchMax     int64
	batchFlushMS int64
	heartbeatMS  int64

	// stale hosts check in exactly once at startup and then go quiet, so
	// the Agents page's staleness heuristic (last_seen older than 3x the
	// heartbeat interval, floor 5 minutes -- see
	// web/src/routes/agents/+page.svelte) flags them a few minutes into
	// any demo session. They emit no logs and no metrics either: a host
	// whose agent is gone stops producing everything, not just
	// heartbeats.
	stale bool
}

const agentVersion = "0.6.2"

var fleet = []host{
	{
		name: "edge-01", service: "nginx",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 100 << 30,
		ipv4: "10.0.1.11", ipv6: "2600:3c02::f03c:94ff:fe1a:1101",
		cpuBase: 22, memFrac: 0.41, diskFrac: 0.36,
		eventsPerMin: 15, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/nginx/access.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "edge-02", service: "nginx",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 100 << 30,
		ipv4: "10.0.1.12", ipv6: "2600:3c02::f03c:94ff:fe1a:1102",
		cpuBase: 19, memFrac: 0.38, diskFrac: 0.33,
		eventsPerMin: 13, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/nginx/access.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "api-01", service: "api",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 160 << 30,
		ipv4: "10.0.2.21", ipv6: "2600:3c02::f03c:94ff:fe1a:2101",
		cpuBase: 34, memFrac: 0.52, diskFrac: 0.29,
		eventsPerMin: 10, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 1000, batchFlushMS: 3000, heartbeatMS: 60000,
	},
	{
		name: "api-02", service: "api",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 160 << 30,
		ipv4: "10.0.2.22", ipv6: "2600:3c02::f03c:94ff:fe1a:2102",
		cpuBase: 37, memFrac: 0.57, diskFrac: 0.31,
		eventsPerMin: 10, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 1000, batchFlushMS: 3000, heartbeatMS: 60000,
	},
	{
		name: "api-03", service: "api",
		// One host deliberately a release behind, so the Agents page's
		// agent_version column shows a fleet that isn't uniformly
		// upgraded -- the normal state of any real fleet.
		os: "Debian GNU/Linux 12 (bookworm)", kernel: "6.1.0-25-amd64", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 160 << 30,
		ipv4: "10.0.2.23", ipv6: "2600:3c02::f03c:94ff:fe1a:2103",
		cpuBase: 41, memFrac: 0.61, diskFrac: 0.44,
		eventsPerMin: 9, systemPerMin: 0.5,
		agentVersion: "0.5.4", sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 1000, batchFlushMS: 3000, heartbeatMS: 60000,
	},
	{
		name: "worker-01", service: "worker",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.3.31", ipv6: "2600:3c02::f03c:94ff:fe1a:3101",
		cpuBase: 46, memFrac: 0.63, diskFrac: 0.4,
		eventsPerMin: 4.5, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 1000, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "worker-02", service: "worker",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.3.32", ipv6: "2600:3c02::f03c:94ff:fe1a:3102",
		cpuBase: 52, memFrac: 0.71, diskFrac: 0.62,
		// The one host with a real, visible trend: ~4 points of disk a
		// day, so a 7-day backfill window ends with it close to full and
		// the "Disk filling up" alert rule has something true to fire on.
		diskGrowthPerDay: 0.04,
		eventsPerMin:     4.5, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 1000, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "db-01", service: "postgres",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 500 << 30,
		ipv4: "10.0.4.41", ipv6: "2600:3c02::f03c:94ff:fe1a:4101",
		cpuBase: 28, memFrac: 0.74, diskFrac: 0.51,
		eventsPerMin: 6, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/postgresql/postgresql-17-main.log",
		batchMax: 1000, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "cache-01", service: "redis",
		os: "Ubuntu 22.04.5 LTS", kernel: "5.15.0-118-generic", arch: "x86_64",
		cores: 2, memTotal: 8 << 30, diskTot: 50 << 30,
		ipv4: "10.0.4.51", ipv6: "2600:3c02::f03c:94ff:fe1a:5101",
		cpuBase: 11, memFrac: 0.58, diskFrac: 0.18,
		eventsPerMin: 2, systemPerMin: 0.3,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=redis-server.service",
		batchMax: 500, batchFlushMS: 10000, heartbeatMS: 60000,
	},
	{
		name: "mail-01", service: "smtp",
		os: "Debian GNU/Linux 13 (trixie)", kernel: "6.12.9-amd64", arch: "x86_64",
		cores: 2, memTotal: 4 << 30, diskTot: 250 << 30,
		ipv4: "198.51.100.25", ipv6: "2600:3c06::2000:7dff:fe55:2501",
		cpuBase: 14, memFrac: 0.46, diskFrac: 0.57,
		eventsPerMin: 6, systemPerMin: 1.2, // internet-facing: more scan/ssh noise than an internal host
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/opt/stalwart/logs/current.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "arm-build-01", service: "worker",
		// The fleet's one non-x86 host, so `stats count by arch`-style
		// questions and the Hosts page's Architecture row have more than
		// one answer in them.
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "aarch64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.5.61", ipv6: "2600:3c02::f03c:94ff:fe1a:6101",
		cpuBase: 63, memFrac: 0.55, diskFrac: 0.47,
		eventsPerMin: 3, systemPerMin: 0.3,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=buildkite-agent.service",
		batchMax: 1000, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-APP-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 250 << 30,
		ipv4: "10.0.6.71", ipv6: "",
		cpuBase: 26, memFrac: 0.64, diskFrac: 0.42,
		eventsPerMin: 2.5, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-SQL-01", service: "eventlog",
		os: "Windows Server 2019 Standard", kernel: "10.0.17763", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 500 << 30,
		ipv4: "10.0.6.72", ipv6: "",
		cpuBase: 33, memFrac: 0.78, diskFrac: 0.66,
		eventsPerMin: 2, systemPerMin: 0,
		agentVersion: "0.5.4", sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "legacy-01", service: "nginx",
		os: "Ubuntu 20.04.6 LTS", kernel: "5.4.0-192-generic", arch: "x86_64",
		cores: 2, memTotal: 4 << 30, diskTot: 40 << 30,
		ipv4: "10.0.1.19", ipv6: "",
		cpuBase: 3, memFrac: 0.22, diskFrac: 0.71,
		eventsPerMin: 0, systemPerMin: 0,
		agentVersion: "0.4.9", sourceKind: "file", sourceDetail: "/var/log/nginx/access.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
		stale: true,
	},
}

// linuxHosts is every host whose agent tails journald or a file -- i.e.
// everything that also produces the `system` service stream. Windows
// hosts produce eventlog records instead, and the stale host produces
// nothing at all.
func linuxHosts() []*host {
	var out []*host
	for i := range fleet {
		h := &fleet[i]
		if h.stale || h.service == "eventlog" {
			continue
		}
		out = append(out, h)
	}
	return out
}
