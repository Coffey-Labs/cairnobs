// The browser side of obs-beacond's API (internal/server/api.go).

export type Severity = 'good' | 'warning' | 'serious' | 'critical' | 'off' | 'waiting';

export interface ContainerSummary { running: number; problems: number; stopped: number; ci: number; total: number }
export interface Disk { mount: string; fs: string; size: number; used: number; pct: number }
export interface Host {
	name: string;
	status: Severity;
	word: string;
	last_seen: string | null;
	silent: boolean;
	agent?: string;
	kernel?: string;
	os?: string;
	uptime_s: number;
	cpus: number;
	cpu_pct: number;
	load: [number, number, number];
	mem_pct: number;
	mem_total: number;
	swap_pct: number;
	disks: Disk[];
	fullest?: Disk;
	net: { rx_bps: number; tx_bps: number };
	history: { cpu: number[]; mem: number[]; rx: number[]; tx: number[] };
	containers: ContainerSummary;
	containers_error?: string;
	units?: UnitView[];
	/** A Windows host's event log: errors and criticals of the last day (docs/windows.md, step 7). */
	events?: HostEvents;
	/** The VM this host is, linked by its SMBIOS UUID. */
	vm?: VMLink;
	vms?: HostVMCount;
}
export interface HostEvents { at: string; since: string; list: EventGroup[]; error?: string }
export interface EventGroup { log: string; source: string; id: number; level: 'critical' | 'error'; count: number; first: string; last: string; message?: string }
/** A cluster's VM, as linked to a host (internal/server/vmlink.go). */
export interface VMLink { source: string; namespace?: string; name: string; node?: string; state: string; word: string; href: string; on_host?: boolean; title?: string }
/** The libvirt VMs a host runs. */
export interface HostVMCount { total: number; running: number; error?: string; href: string }
export interface UnitView { name: string; load: string; active: string; sub: string; restarts: number; since?: string; host: string; severity: Severity; word: string }
export interface Attention {
	severity: Severity; title: string; detail: string; href: string; host: string;
	key?: string; affected?: string;
	/** The open incident it is, once there is one: the dashboard's "I'm on it" acknowledges it. */
	incident?: { id: string; opened: string; fired: string | null; ack: { by: string; at: string } | null };
}
/** What happened since this person last looked (internal/server/today.go). */
export interface Today { since: string; last_visit: boolean; events: TodayEvent[]; opened: number; resolved: number; fixed_itself: number; told: number }
export interface TodayEvent { id: string; severity: Severity; title: string; host?: string; href?: string; at: string; opened: string; resolved?: string; ack_by?: string; told: boolean }
/** A service's last 24 hours as 48 half-hour cells (the worst status in each, '' unknown), and its uptime (-1 unknown). */
export interface ServiceHistory { cells: string[]; pct_24h: number; pct_30d: number }
export interface ServiceBrief { id: string; name: string; tier: string; status: Severity; word: string; history: ServiceHistory }
/** The few hosts heading somewhere, in place of gauges. */
export interface Watch {
	filling: { host: string; mount: string; pct: number; full_days: number }[];
	busier: { host: string; what: 'cpu' | 'memory'; now: number; usual: number }[];
	restarted: { host: string; up_s: number }[];
	restarted_n: number;
}
/** Every host in a few numbers (internal/server/lists.go). */
export interface Fleet {
	hosts: number;
	reporting: number;
	busiest?: { host: string; cpu_pct: number };
	fullest?: { host: string; mount: string; pct: number };
	container_severity: Severity;
	/** Hosts needing a look. */
	warning?: number;
	critical?: number;
}
/** A row of the hosts list: GET /api/v1/hosts. */
export interface HostRow {
	name: string; status: Severity; word: string; last_seen: string | null; silent: boolean; workstation?: boolean;
	os?: string; uptime_s: number; cpu_pct: number; mem_pct: number; fullest?: Disk; containers: ContainerSummary;
}
/** hosts is every host in a small fleet, else the ones most in need; fleet counts them all. */
export interface Overview { now: string; version: string; hosts: Host[]; fleet: Fleet; attention: Attention[]; containers: ContainerSummary; alerts: { firing: number; unacknowledged: number }; checks?: { down: number; severity: string }; features?: Features; clusters?: Cluster[]; today?: Today; services?: ServiceBrief[]; watch?: Watch }
/** A cluster agent's cluster: GET /api/v1/clusters, and the overview. */
export interface Cluster {
	name: string; status: Severity; word: string; heard: string | null; listed: string | null; agent?: string; version?: string;
	nodes: number; nodes_ready: number; workloads: number; problems: number; kinds: Record<string, Record<string, number>>;
	parts?: ClusterPart[]; operators?: number; operators_ok?: number; title?: string; read_by?: string;
	/** An array's performance and space now; history is its name in GET /api/v1/workloads/hours. */
	usage?: Usage; history?: string;
	/** What it is (Cluster, Array, vCenter, Prism Central) and what its nodes are (Node, Controller, Host). */
	noun: string; node_noun: string;
	/** The Clusters page's section (Kubernetes, Virtualization, Storage, IBM i, Secrets), and its state in a line, in its kind's terms. */
	group: string; summary: string;
	/** An IBM i partition's own numbers, in place of nodes. */
	partition?: PartitionNums;
}
/** An IBM i partition now: CPU over the last minute (none until there's a minute to tell), its system ASP and jobs. */
export interface PartitionNums { cpu_pct?: number; system_asp_pct: number; active_jobs: number; jobs: number; model?: string; processing_units?: number;
	subsystems: number; subsystems_active: number; interfaces: number; interfaces_active: number;
	/** The Power server it runs on, when an HMC lists it. */
	server?: { name: string; state: string; word: string; hmc: string; partition: string; href: string } }
/** One of an OpenShift or OKD cluster's own parts: its version, a machine config pool, an operator. */
export interface ClusterPart { name: string; title: string; state: string; word: string; severity: Severity }
export interface ClusterNode { name: string; state: string; severity: Severity; word: string; since?: string; facts?: Record<string, unknown> }
/** A row of the workloads list: GET /api/v1/workloads. */
export interface Workload {
	source: string; kind: string; id: string; name: string; namespace?: string; owner?: string; node?: string;
	state: string; severity: Severity; word: string; since?: string; alert?: string;
	labels?: Record<string, string>; facts?: Record<string, unknown>;
	/** For a VM: the host agent inside it, linked by its SMBIOS UUID. */
	host?: { name: string; href: string };
	/** For a PVC: the array volumes behind it; for an array volume, the PVCs it serves (by PersistentVolume name). */
	volumes?: { source: string; array: string; name: string; state: string; word: string; href: string }[];
	pvcs?: { source: string; namespace: string; name: string; state: string; href: string }[];
	/** CPU (cores) and memory (bytes) now, for nodes, controllers and VMs; *_of is the ceiling, when there is one. */
	usage?: Usage;
}
export interface Usage { cpu: number; mem: number; cpu_of?: number; mem_of?: number; cpu_pct?: number; mem_pct?: number; io?: IO; leases?: number; at: string }
/** An array's reads and writes now (a second; microseconds each; bytes a second) and the space it uses. */
export interface IO { read_ops: number; write_ops: number; read_us: number; write_us: number; read_bps: number; write_bps: number; used: number; used_of?: number }
/** One certificate in the inventory: GET /api/v1/certificates. */
export interface Certificate {
	fingerprint: string; subject: string; names: string[]; issuer: string; not_before: string; not_after: string;
	key: string; sig: string; ca?: boolean; days: number; status: Severity; word: string; weak?: string; renewals?: number;
	places: { kind: string; where: string; href: string }[]; place_count: number; kinds: string[];
	/** Ports still serving it though a renewed one is in their host's file, and that file. */
	not_reloaded?: string[]; renewed?: string;
	/** What's wrong with how it's served, by place: a name it doesn't cover, a missing intermediate. */
	wrong?: string[];
	/** Reloads an admin may ask for (the host's cert_reload lists the unit), and the last one asked. */
	reloads?: CertReload[];
	/** cert-manager Certificates behind it an admin may ask to renew now (the cluster agent runs with -cert-renew). */
	renews?: CertRenew[];
}
export interface CertReload { host: string; unit: string; addr: string; port: number; where: string; file: string; last?: Action }
export interface CertRenew { source: string; namespace: string; certificate: string; where: string; last?: Action }
/** An action handed to an agent (docs/cert-rotation.md): GET /api/v1/actions. */
export interface Action {
	id: string; agent: string; kind: string; args: unknown; by: string; created: string;
	state: 'waiting' | 'taken' | 'done' | 'failed' | 'expired'; detail?: string; at?: string;
}
/** One hour of a workload's usage: GET /api/v1/workloads/hours. */
export interface WorkloadHour {
	hour: string; cpu: number; cpu_max: number; cpu_of?: number; mem: number; mem_max: number; mem_of?: number;
	/** An array's hour: averages, the worst reading's latency, and the space used at its end. */
	read_ops?: number; write_ops?: number; read_us?: number; read_us_max?: number; write_us?: number; write_us_max?: number;
	read_bps?: number; write_bps?: number; used?: number; used_of?: number;
	/** A Vault cluster's hour: its lease count, average and peak. */
	leases?: number; leases_max?: number;
}
export interface Incident {
	id: string;
	key: string;
	severity: Severity;
	title: string;
	detail: string;
	host: string;
	href: string;
	opened: string;
	fired: string | null;
	notified: string | null;
	resolved: string | null;
	ack: { by: string; at: string } | null;
	logs?: string[];
	affected?: string;
	/** The learned dependencies that grouped other alerts under it (docs/maps.md). */
	grouped?: { from: string; to: string; from_name: string; to_name: string; port?: number; since: string }[];
	/** Where it is on the maps: a page path with its focus. */
	map?: string;
	/** Where the problem is: hosts, clusters, certificates, checks, mail, data, backups, cloudflare, akamai. */
	source?: string;
}
export interface AlertChannel { id: string; type: string; min: string }
export interface CfOrigin { name: string; address: string; enabled: boolean; healthy: boolean; reason?: string; code?: number; rtt?: string }
export interface CfPool { name: string; enabled: boolean; healthy: boolean; origins: CfOrigin[] }
export interface CfTraffic { requests: number; errors: number; error_hosts: { host: string; count: number }[]; blocked: number; blocked_day: number }
export interface CfZone { name: string; ssl: string; certs: { type: string; status: string; hosts: string[]; expires?: string }[]; load_balancers: string[]; traffic?: CfTraffic; dns_records: number }
export interface Cloudflare {
	configured: boolean;
	snapshot?: { checked: string; error?: string; error_since?: string; traffic_error?: string; zones: CfZone[]; pools: CfPool[]; status: { indicator: string; description: string } };
	conditions?: Attention[];
	dns_changes?: Record<string, { added: string[]; removed: string[] }>; error_warn_pct?: number; error_critical_pct?: number;
	ssl_mode?: string; ssl_mode_zones?: Record<string, string> | null; cert_warn_days?: number; cert_critical_days?: number; every_s: number;
}
export interface AkServer { ip: string; alive: boolean; handed_out: boolean; score: number; known: boolean }
export interface AkTarget { datacenter: string; id: number; enabled: boolean; weight: number; servers: AkServer[] }
export interface AkProperty { name: string; type?: string; targets: AkTarget[]; at: string; error?: string }
export interface AkDomain { name: string; type?: string; propagation?: string; since: string; message?: string; properties: AkProperty[]; error?: string }
/** A deployed certificate as Akamai's CPS has it. */
export interface AkCert { fingerprint: string; subject: string; names: string[]; issuer: string; not_before: string; not_after: string; key: string; sig: string }
/** A CPS enrollment: `waiting` is what a pending change needs from the customer. */
export interface AkEnrollment { id: number; cn: string; sans?: string[]; validation?: string; type?: string; pending: number; waiting?: string; deployed: AkCert[]; error?: string }
/** A Property Manager activation. */
export interface AkActivation { id: string; version: number; type: string; status: string; submitted: string; updated: string; note?: string }
/** A Property Manager property: versions active on each network, and the newest activation there. */
export interface AkPMProperty { id: string; name: string; group: string; latest: number; staging: number; production: number; last_staging?: AkActivation; last_production?: AkActivation; read: string; error?: string }
/** An Edge DNS zone: `count` record sets (not sent; their changes are). */
export interface AkDNSZone { name: string; type: string; version: string; modified: string; count: number; error?: string }
/** GET /api/v1/akamai (docs/akamai.md). */
export interface Akamai {
	configured: boolean;
	snapshot?: { checked: string; error?: string; error_since?: string; domains: AkDomain[]; enrollments?: AkEnrollment[] | null; cps_at?: string; cps_error?: string; pm_properties?: AkPMProperty[] | null; papi_at?: string; papi_error?: string; dns_zones?: AkDNSZone[] | null; dns_at?: string; dns_error?: string;
		traffic?: { cpcode: number; hits: number; errors: number }[] | null; traffic_start?: string; traffic_end?: string; traffic_error?: string;
		status?: { indicator: string; description: string }; absent?: string[] };
	hosts?: Record<string, string>; certs?: Record<string, { status: Severity; word: string; weak?: string }>; dns_changes?: Record<string, { added: string[]; removed: string[] }>; conditions?: Attention[]; every_s: number;
	error_warn_pct?: number; error_critical_pct?: number; error_min?: number;
}
export interface CheckResult { id: string; at: string; ok: boolean; error?: string; ms: number; status?: number; answer?: string; cert_expires?: string; loss_pct?: number }
export interface CheckView { id: string; name: string; kind: string; target: string; status: string; word: string; detail?: string; cert_days?: number; results: Record<string, CheckResult>; from: string[] }
export interface Checks { checks: CheckView[]; configured: boolean; source: Source; file: string }
/** Where a section comes from: a config file (read-only here), the UI (kept in the database), or nowhere (no database). */
export type Source = 'file' | 'ui' | '';
export interface CheckForm {
	id: string; name: string; kind: string; target: string; lists: string[]; status: number | null; contains: string; tls: boolean;
	type: string; server: string; every_s: number | null; timeout_s: number | null; from: string[]; severity: string;
	tls_warn_days: number | null; tls_critical_days: number | null; // null (0 from the server): the default
}
export interface CheckSettings { source: Source; file: string; checks: CheckForm[]; hosts: string[] }
export interface WindowForm { name: string; hosts: string[]; daily: string; timezone: string; duration_s: number }
export interface WindowSettings { source: Source; windows: WindowForm[]; hosts: string[] }
/** An alert channel as the UI edits it. Secrets only go in: `set` names the stored ones, never their values. */
export interface ChannelForm {
	id: string; type: string; min: string; server: string; mention_critical: boolean; smtp: string; username: string; from: string; to: string[];
	set: string[]; secrets?: Record<string, string>; clear?: string[];
}
export interface ChannelSettings { source: Source; channels: ChannelForm[] }
/** One host's lists in expected.yaml: containers that must run, ones stopped on purpose, services that must stay active. */
export interface HostExpectations { running: string[]; stopped: { match: string; note: string }[]; units: string[] }
/** What should run on a host, and what its agent says is there now (null: it can't say). */
export interface ExpectedSettings {
	source: Source; file: string; host: string; reported: boolean; expected: HostExpectations; ci_jobs: string[];
	containers: { name: string; state: string; status: string }[] | null; containers_error?: string; services: string[] | null; windows?: boolean;
}
export interface AuditEntry { at: string; who: string; action: string; detail: string }
export interface Audit { entries: AuditEntry[]; stored: boolean }
export interface Incidents { open: Incident[]; recent: Incident[]; channels: AlertChannel[]; for_s: number; repeat_s: number; stored: boolean; maintenance?: MaintWindow[]; maintenance_source?: Source; channels_source?: Source }
export interface MaintWindow { name: string; hosts: string[]; daily: string; timezone: string; duration_s: number; active: boolean; next: string }
export interface Container {
	id: string;
	name: string;
	image: string;
	project?: string;
	state: string;
	status: string;
	health?: string;
	created: number;
	host: string;
	expected: boolean;
	verdict: { severity: Severity; word: string; detail?: string };
	logs?: string[];
}
export interface Provider { id: string; label: string; kind?: 'redirect' | 'passkey'; note?: string; passkeys?: boolean; passwords?: boolean }
export interface SigninOptions { signin: { dev: boolean; demo?: boolean; providers: Provider[] } }
export interface Session { user: string; email?: string; role: string; provider?: string; dev: boolean; demo?: boolean; version: string; door?: boolean }

export class SignedOut extends Error {
	constructor(public options: SigninOptions) {
		super('signed out');
	}
}

export async function get<T>(url: string): Promise<T> {
	const r = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } });
	if (r.status === 401) throw new SignedOut(await r.json());
	if (!r.ok) throw await failed(r);
	return r.json() as Promise<T>;
}

/**
 * An error obs-beacond answered with. Its text is the server's own message
 * ("This is the demo: it shows everything and changes nothing.") when there
 * is one, and String(e) gives just that, without "Error: ".
 */
export class APIError extends Error {
	status: number;
	constructor(message: string, status: number) {
		super(message);
		this.status = status;
	}
	toString() {
		return this.message;
	}
}

async function failed(r: Response): Promise<APIError> {
	const j = (await r.json().catch(() => ({}))) as { error?: string };
	return new APIError(j.error || `The server answered ${r.status} ${r.statusText}`.trim(), r.status);
}

/** POSTs carry X-Beacon, which obs-beacond requires and a cross-site form can't add. */
export async function post(url: string): Promise<void> {
	const r = await fetch(url, { method: 'POST', credentials: 'same-origin', headers: { 'X-Beacon': '1' } });
	if (!r.ok) throw await failed(r);
}

/** A JSON request that changes something (PUT, POST with a body), with X-Beacon. */
export async function send<T = unknown>(url: string, method: string, body?: unknown): Promise<T> {
	const r = await fetch(url, {
		method, credentials: 'same-origin',
		headers: { 'X-Beacon': '1', 'Content-Type': 'application/json', Accept: 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
	const j = await r.json().catch(() => ({}));
	if (!r.ok) throw new APIError((j as { error?: string }).error || `The server answered ${r.status} ${r.statusText}`.trim(), r.status);
	return j as T;
}

/** Replaces a whole settings section with a YAML document (import). */
export async function sendYAML<T = unknown>(url: string, yaml: string): Promise<T> {
	const r = await fetch(url, {
		method: 'POST', credentials: 'same-origin',
		headers: { 'X-Beacon': '1', 'Content-Type': 'application/yaml', Accept: 'application/json' },
		body: yaml
	});
	const j = await r.json().catch(() => ({}));
	if (!r.ok) throw new APIError((j as { error?: string }).error || `The server answered ${r.status} ${r.statusText}`.trim(), r.status);
	return j as T;
}

/** Which monitoring pages have something to show. */
export interface Features { maps: boolean; topology?: boolean; mail: boolean; data: boolean; containers: boolean; backups: boolean; cloudflare: boolean; akamai?: boolean; checks: boolean; reports: boolean; workloads?: boolean; certificates?: boolean }

/** The Configure overview: each area, where it's set and how much is in it. */
export interface SettingsSummary {
	database: boolean;
	hosts: number;
	clusters: number;
	sections: Record<'checks' | 'maintenance' | 'channels' | 'expected', { source: Source; file?: string; count: number }>;
	integrations: Record<'cloudflare' | 'mail' | 'datalayer' | 'backups' | 'topology' | 'digest', Source>;
	signin: string[];
}

/** One box on a discovered map (docs/maps.md). */
export interface MapBox {
	id: string;
	kind: string; // internet, site, cluster, host, group, service, elsewhere:<kind>
	name: string;
	sub?: string;
	status: Severity;
	count?: number;
	problems?: string[];
	ports?: number[];
	open?: boolean;
	tier?: string;
	layer: number;
	site?: string;
	via?: string;
	detail?: Record<string, unknown>;
}
export interface MapLine { a: string; b: string; kind: string; up: boolean; ports?: number[]; count?: number; stage?: string }
/** One level of a discovered map, already grouped by obs-beacond. */
export interface MapView {
	built: string;
	level: string;
	focus?: string;
	title: string;
	crumbs: MapBox[];
	boxes: MapBox[];
	lines: MapLine[];
	cut?: boolean;
	hosts: number;
	mapped: number;
}
export interface MapFound { kind: string; name: string; where?: string; map: 'network' | 'services'; focus: string }

/** One correction to the maps (docs/maps.md, Corrections); op says which. */
export interface MapFixReq {
	op: 'name' | 'tier' | 'hide' | 'show' | 'merge' | 'move' | 'edge' | 'outside' | 'drop-outside';
	id?: string;
	name?: string;
	tier?: string;
	into?: string;
	host?: string;
	owner?: string;
	from?: string;
	to?: string;
	port?: number;
	verdict?: '' | 'confirm' | 'reject';
	outside?: { id?: string; name: string; addrs?: string[]; needed_by?: string[]; note?: string };
}
export interface MapFixItem { kind: string; text: string; undo: MapFixReq }
export interface MapFixes {
	editable: boolean;
	items: MapFixItem[] | null;
	topology: boolean;
	updated?: string;
	updated_by?: string;
}
export interface MapFoundService { id: string; name: string; kind?: string; hosts: string[]; first_seen: string }
export interface MapFoundEdge { from: string; to: string; from_name: string; to_name: string; port: number; stage: string; first_seen: string }
export interface MapFoundList {
	since: string;
	services: MapFoundService[] | null;
	edges: MapFoundEdge[] | null;
	service_count: number;
	edge_count: number;
}
