package main

import "strings"

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
	// Fifty hosts, shaped like an estate rather than a stack: a proxy
	// tier in front, application and worker tiers behind it, the data
	// services they lean on, a platform tier (Kubernetes, CI, secrets,
	// directory, DNS, backup), and a Windows tier that is a tier rather
	// than a token box. Thirty-one Linux, eighteen Windows, and one
	// Linux host whose agent is gone.
	//
	// The proportions matter more than the count: an enterprise looking
	// at this should recognise its own estate, which means Windows
	// carrying real services (AD, IIS, SQL Server, Exchange, file, RDS,
	// print, WSUS/SCCM) instead of appearing only as a Security channel.
	{
		name: "lb-01", service: "haproxy",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 60 << 30,
		ipv4: "10.0.1.5", ipv6: "2600:3c02::f03c:94ff:fe1a:2001",
		cpuBase: 26, memFrac: 0.36, diskFrac: 0.28,
		eventsPerMin: 14, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/haproxy.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "lb-02", service: "haproxy",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 60 << 30,
		ipv4: "10.0.1.6", ipv6: "2600:3c02::f03c:94ff:fe1a:2002",
		cpuBase: 24, memFrac: 0.34, diskFrac: 0.27,
		eventsPerMin: 13, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/haproxy.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
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
		name: "proxy-01", service: "squid",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 2, memTotal: 4 << 30, diskTot: 80 << 30,
		ipv4: "10.0.3.10", ipv6: "2600:3c02::f03c:94ff:fe1a:3001",
		cpuBase: 12, memFrac: 0.29, diskFrac: 0.44,
		eventsPerMin: 6, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/squid/access.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "api-01", service: "api",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.2.21", ipv6: "2600:3c02::f03c:94ff:fe1a:2201",
		cpuBase: 44, memFrac: 0.62, diskFrac: 0.41,
		eventsPerMin: 22, systemPerMin: 0.8,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "api-02", service: "api",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.2.22", ipv6: "2600:3c02::f03c:94ff:fe1a:2202",
		cpuBase: 47, memFrac: 0.62, diskFrac: 0.41,
		eventsPerMin: 24, systemPerMin: 0.8,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "api-03", service: "api",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.2.23", ipv6: "2600:3c02::f03c:94ff:fe1a:2203",
		cpuBase: 41, memFrac: 0.62, diskFrac: 0.41,
		eventsPerMin: 21, systemPerMin: 0.8,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "api-04", service: "api",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.2.24", ipv6: "2600:3c02::f03c:94ff:fe1a:2204",
		cpuBase: 39, memFrac: 0.62, diskFrac: 0.41,
		eventsPerMin: 19, systemPerMin: 0.8,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-api.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "worker-01", service: "worker",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.2.41", ipv6: "2600:3c02::f03c:94ff:fe1a:2241",
		cpuBase: 31, memFrac: 0.55, diskFrac: 0.48,
		eventsPerMin: 9, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "worker-02", service: "worker",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.2.42", ipv6: "2600:3c02::f03c:94ff:fe1a:2242",
		cpuBase: 52, memFrac: 0.71, diskFrac: 0.62,
		// The disk-filling story the worker-disk-filling alert rule fires
		// on. It has to stay on this host and at this rate: the rule names
		// worker-02 and thresholds on 175 GiB of its 200.
		diskGrowthPerDay: 0.04,
		eventsPerMin:     8, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "worker-03", service: "worker",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.2.43", ipv6: "2600:3c02::f03c:94ff:fe1a:2243",
		cpuBase: 33, memFrac: 0.55, diskFrac: 0.48,
		eventsPerMin: 9, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "arm-build-01", service: "worker",
		os: "Debian GNU/Linux 12 (bookworm)", kernel: "6.1.0-27-arm64", arch: "aarch64",
		cores: 8, memTotal: 16 << 30, diskTot: 300 << 30,
		ipv4: "10.0.2.51", ipv6: "2600:3c02::f03c:94ff:fe1a:2251",
		cpuBase: 55, memFrac: 0.61, diskFrac: 0.52,
		eventsPerMin: 7, systemPerMin: 0.8,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-worker.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "db-01", service: "postgres",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 1000 << 30,
		ipv4: "10.0.4.11", ipv6: "2600:3c02::f03c:94ff:fe1a:4001",
		cpuBase: 38, memFrac: 0.71, diskFrac: 0.58,
		eventsPerMin: 18, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=postgresql@16-main.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "db-02", service: "postgres",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 1000 << 30,
		ipv4: "10.0.4.12", ipv6: "2600:3c02::f03c:94ff:fe1a:4002",
		cpuBase: 22, memFrac: 0.66, diskFrac: 0.57,
		eventsPerMin: 9, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=postgresql@16-main.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "mysql-01", service: "mysql",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 600 << 30,
		ipv4: "10.0.4.13", ipv6: "2600:3c02::f03c:94ff:fe1a:4003",
		cpuBase: 29, memFrac: 0.68, diskFrac: 0.49,
		eventsPerMin: 11, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=mysql.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "cache-01", service: "redis",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 60 << 30,
		ipv4: "10.0.4.21", ipv6: "2600:3c02::f03c:94ff:fe1a:4011",
		cpuBase: 9, memFrac: 0.44, diskFrac: 0.19,
		eventsPerMin: 7, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=redis-server.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "cache-02", service: "redis",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 60 << 30,
		ipv4: "10.0.4.22", ipv6: "2600:3c02::f03c:94ff:fe1a:4012",
		cpuBase: 8, memFrac: 0.42, diskFrac: 0.18,
		eventsPerMin: 6, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=redis-server.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "mq-01", service: "rabbitmq",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.4.31", ipv6: "2600:3c02::f03c:94ff:fe1a:4021",
		cpuBase: 17, memFrac: 0.51, diskFrac: 0.26,
		eventsPerMin: 10, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=rabbitmq-server.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "mq-02", service: "rabbitmq",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.4.32", ipv6: "2600:3c02::f03c:94ff:fe1a:4022",
		cpuBase: 15, memFrac: 0.49, diskFrac: 0.25,
		eventsPerMin: 9, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=rabbitmq-server.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "search-01", service: "elasticsearch",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 800 << 30,
		ipv4: "10.0.4.41", ipv6: "2600:3c02::f03c:94ff:fe1a:4031",
		cpuBase: 41, memFrac: 0.79, diskFrac: 0.62,
		eventsPerMin: 12, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=elasticsearch.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "search-02", service: "elasticsearch",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 800 << 30,
		ipv4: "10.0.4.42", ipv6: "2600:3c02::f03c:94ff:fe1a:4032",
		cpuBase: 39, memFrac: 0.77, diskFrac: 0.61,
		eventsPerMin: 11, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=elasticsearch.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "k8s-node-01", service: "kubelet",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 400 << 30,
		ipv4: "10.0.7.11", ipv6: "2600:3c02::f03c:94ff:fe1a:7011",
		cpuBase: 52, memFrac: 0.73, diskFrac: 0.44,
		eventsPerMin: 16, systemPerMin: 0.9,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=kubelet.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "k8s-node-02", service: "kubelet",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 400 << 30,
		ipv4: "10.0.7.12", ipv6: "2600:3c02::f03c:94ff:fe1a:7012",
		cpuBase: 49, memFrac: 0.73, diskFrac: 0.44,
		eventsPerMin: 15, systemPerMin: 0.9,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=kubelet.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "k8s-node-03", service: "kubelet",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 400 << 30,
		ipv4: "10.0.7.13", ipv6: "2600:3c02::f03c:94ff:fe1a:7013",
		cpuBase: 47, memFrac: 0.73, diskFrac: 0.44,
		eventsPerMin: 14, systemPerMin: 0.9,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=kubelet.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "ci-01", service: "jenkins",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 500 << 30,
		ipv4: "10.0.7.21", ipv6: "2600:3c02::f03c:94ff:fe1a:7021",
		cpuBase: 58, memFrac: 0.64, diskFrac: 0.69,
		eventsPerMin: 8, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=jenkins.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "vault-01", service: "vault",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 2, memTotal: 8 << 30, diskTot: 40 << 30,
		ipv4: "10.0.7.31", ipv6: "2600:3c02::f03c:94ff:fe1a:7031",
		cpuBase: 7, memFrac: 0.31, diskFrac: 0.21,
		eventsPerMin: 5, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=vault.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "ldap-01", service: "openldap",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 2, memTotal: 8 << 30, diskTot: 40 << 30,
		ipv4: "10.0.7.41", ipv6: "2600:3c02::f03c:94ff:fe1a:7041",
		cpuBase: 11, memFrac: 0.34, diskFrac: 0.23,
		eventsPerMin: 7, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=slapd.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "dns-01", service: "bind",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 2, memTotal: 4 << 30, diskTot: 30 << 30,
		ipv4: "10.0.5.2", ipv6: "2600:3c02::f03c:94ff:fe1a:5001",
		cpuBase: 8, memFrac: 0.27, diskFrac: 0.17,
		eventsPerMin: 12, systemPerMin: 0.4,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=named.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "backup-01", service: "backup",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 4000 << 30,
		ipv4: "10.0.8.11", ipv6: "2600:3c02::f03c:94ff:fe1a:8001",
		cpuBase: 19, memFrac: 0.41, diskFrac: 0.74,
		diskGrowthPerDay: 0.012,
		eventsPerMin:     4, systemPerMin: 0.5,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/log/bacula/bacula.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "mail-01", service: "smtp",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 200 << 30,
		ipv4: "10.0.6.11", ipv6: "2600:3c02::f03c:94ff:fe1a:6001",
		cpuBase: 16, memFrac: 0.47, diskFrac: 0.39,
		eventsPerMin: 9, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=stalwart-mail.service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "DC-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 300 << 30,
		ipv4: "10.0.6.21", ipv6: "",
		cpuBase: 26, memFrac: 0.61, diskFrac: 0.44,
		eventsPerMin: 6, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application,Directory Service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "DC-02", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 300 << 30,
		ipv4: "10.0.6.22", ipv6: "",
		cpuBase: 24, memFrac: 0.59, diskFrac: 0.43,
		eventsPerMin: 5.5, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application,Directory Service",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "IIS-01", service: "iis",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 250 << 30,
		ipv4: "10.0.6.31", ipv6: "",
		cpuBase: 34, memFrac: 0.58, diskFrac: 0.47,
		eventsPerMin: 12, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "IIS-02", service: "iis",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 250 << 30,
		ipv4: "10.0.6.32", ipv6: "",
		cpuBase: 32, memFrac: 0.56, diskFrac: 0.46,
		eventsPerMin: 11, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "IIS-03", service: "iis",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 250 << 30,
		ipv4: "10.0.6.33", ipv6: "",
		cpuBase: 30, memFrac: 0.55, diskFrac: 0.45,
		eventsPerMin: 10, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-SQL-01", service: "mssql",
		os: "Windows Server 2019 Standard", kernel: "10.0.17763", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 2000 << 30,
		ipv4: "10.0.6.72", ipv6: "",
		cpuBase: 43, memFrac: 0.81, diskFrac: 0.68,
		eventsPerMin: 9, systemPerMin: 0,
		agentVersion: "0.5.4", sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-SQL-02", service: "mssql",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 2000 << 30,
		ipv4: "10.0.6.73", ipv6: "",
		cpuBase: 39, memFrac: 0.79, diskFrac: 0.66,
		eventsPerMin: 8, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "EXCH-01", service: "exchange",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 1500 << 30,
		ipv4: "10.0.6.41", ipv6: "",
		cpuBase: 37, memFrac: 0.74, diskFrac: 0.71,
		eventsPerMin: 10, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "EXCH-02", service: "exchange",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 1500 << 30,
		ipv4: "10.0.6.42", ipv6: "",
		cpuBase: 34, memFrac: 0.72, diskFrac: 0.69,
		eventsPerMin: 9, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "FS-01", service: "smb",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 8000 << 30,
		ipv4: "10.0.6.51", ipv6: "",
		cpuBase: 14, memFrac: 0.48, diskFrac: 0.79,
		eventsPerMin: 8, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "FS-02", service: "smb",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 8000 << 30,
		ipv4: "10.0.6.52", ipv6: "",
		cpuBase: 12, memFrac: 0.46, diskFrac: 0.83,
		diskGrowthPerDay: 0.008,
		eventsPerMin:     7, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "RDS-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 400 << 30,
		ipv4: "10.0.6.61", ipv6: "",
		cpuBase: 48, memFrac: 0.77, diskFrac: 0.51,
		eventsPerMin: 7, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application,TerminalServices",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "RDS-02", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 16, memTotal: 64 << 30, diskTot: 400 << 30,
		ipv4: "10.0.6.62", ipv6: "",
		cpuBase: 45, memFrac: 0.75, diskFrac: 0.49,
		eventsPerMin: 6.5, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application,TerminalServices",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-APP-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 200 << 30,
		ipv4: "10.0.6.71", ipv6: "",
		cpuBase: 18, memFrac: 0.52, diskFrac: 0.4,
		eventsPerMin: 2.5, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WIN-APP-02", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 200 << 30,
		ipv4: "10.0.6.74", ipv6: "",
		cpuBase: 16, memFrac: 0.5, diskFrac: 0.38,
		eventsPerMin: 2.2, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "PRINT-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 4, memTotal: 8 << 30, diskTot: 120 << 30,
		ipv4: "10.0.6.81", ipv6: "",
		cpuBase: 9, memFrac: 0.38, diskFrac: 0.31,
		eventsPerMin: 3, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application,PrintService",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "WSUS-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 4, memTotal: 16 << 30, diskTot: 900 << 30,
		ipv4: "10.0.6.91", ipv6: "",
		cpuBase: 11, memFrac: 0.44, diskFrac: 0.72,
		eventsPerMin: 3.5, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "SCCM-01", service: "eventlog",
		os: "Windows Server 2022 Datacenter", kernel: "10.0.20348", arch: "x86_64",
		cores: 8, memTotal: 32 << 30, diskTot: 1200 << 30,
		ipv4: "10.0.6.92", ipv6: "",
		cpuBase: 21, memFrac: 0.57, diskFrac: 0.63,
		eventsPerMin: 4, systemPerMin: 0,
		agentVersion: agentVersion, sourceKind: "eventlog", sourceDetail: "channels=Security,System,Application",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "shop-mag-01", service: "magento",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 32 << 30, diskTot: 400 << 30,
		ipv4: "10.0.9.11", ipv6: "2600:3c02::f03c:94ff:fe1a:9011",
		cpuBase: 51, memFrac: 0.69, diskFrac: 0.46,
		eventsPerMin: 26, systemPerMin: 0.9,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/www/shop/var/log/system.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "shop-mag-02", service: "magento",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 16, memTotal: 32 << 30, diskTot: 400 << 30,
		ipv4: "10.0.9.12", ipv6: "2600:3c02::f03c:94ff:fe1a:9012",
		cpuBase: 48, memFrac: 0.67, diskFrac: 0.44,
		eventsPerMin: 24, systemPerMin: 0.9,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/www/shop/var/log/system.log",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "shop-woo-01", service: "woocommerce",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 200 << 30,
		ipv4: "10.0.9.21", ipv6: "2600:3c02::f03c:94ff:fe1a:9021",
		cpuBase: 37, memFrac: 0.58, diskFrac: 0.39,
		eventsPerMin: 17, systemPerMin: 0.7,
		agentVersion: agentVersion, sourceKind: "file", sourceDetail: "/var/www/woo/wp-content/uploads/wc-logs/",
		batchMax: 500, batchFlushMS: 5000, heartbeatMS: 60000,
	},
	{
		name: "pay-01", service: "payments",
		os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", arch: "x86_64",
		cores: 8, memTotal: 16 << 30, diskTot: 120 << 30,
		ipv4: "10.0.9.31", ipv6: "2600:3c02::f03c:94ff:fe1a:9031",
		cpuBase: 23, memFrac: 0.47, diskFrac: 0.28,
		eventsPerMin: 19, systemPerMin: 0.6,
		agentVersion: agentVersion, sourceKind: "journald", sourceDetail: "unit=shop-payments.service",
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

// windows reports whether this host's agent reads Windows channels
// rather than journald or a file.
//
// Decided from `os` rather than from `service`, which is what it used to
// key off. That worked while "eventlog" was the only Windows role; the
// Windows tier now runs IIS, SQL Server, Exchange and file servers, and
// keying off the role would have handed every one of them a journald
// `system` stream -- sshd and UFW lines on a Windows box, which is the
// kind of wrong that is hard to notice and impossible to unsee.
func (h *host) windows() bool { return strings.HasPrefix(h.os, "Windows") }

// linuxHosts is every host whose agent tails journald or a file -- i.e.
// everything that also produces the `system` service stream. Windows
// hosts produce their own channel records instead, and the stale host
// produces nothing at all.
func linuxHosts() []*host {
	var out []*host
	for i := range fleet {
		h := &fleet[i]
		if h.stale || h.windows() {
			continue
		}
		out = append(out, h)
	}
	return out
}
