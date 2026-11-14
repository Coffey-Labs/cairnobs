// Package report is the wire format between obs-beacon-agent and obs-beacond.
// See docs/protocol.md. Field names are part of the protocol: add fields,
// never rename or reuse them, and bump Version for anything incompatible.
package report

import (
	"encoding/json"
	"time"
)

// Version is the protocol version an agent sends in every report.
const Version = 1

// Report is one snapshot of a host, sent by its agent every interval.
type Report struct {
	V        int       `json:"v"`
	Agent    string    `json:"agent"` // agent build version
	Host     string    `json:"host"`  // what the agent calls itself; obs-beacond trusts the token, not this
	Time     time.Time `json:"time"`
	Interval int       `json:"interval_s"`

	Kernel string `json:"kernel"`
	OS     string `json:"os"`
	// Machine is the SMBIOS system UUID, lower case, from the root helper:
	// a hypervisor shows the same number for the VM (KubeVirt's firmware
	// UUID, libvirt's domain UUID), which links the two. Absent without the
	// helper, or when the firmware gives none or a placeholder.
	Machine string     `json:"machine_uuid,omitempty"`
	Uptime  float64    `json:"uptime_s"`
	CPUs    int        `json:"cpus"`
	CPU     float64    `json:"cpu_pct"` // busy share since the previous report
	Load    [3]float64 `json:"load"`
	Mem     Mem        `json:"mem"`
	Disks   []Disk     `json:"disks"`
	Net     Net        `json:"net"`

	Containers *Containers `json:"containers,omitempty"`

	// VMs are the host's libvirt domains (docs/cluster-watcher.md, step 6).
	// Absent where libvirt isn't running, or without the helper.
	VMs *VMs `json:"vms,omitempty"`

	// Units are the systemd services obs-beacond asked this agent to watch.
	Units []Unit `json:"units,omitempty"`

	// Services are the systemd services running on the host, by name, so an
	// admin can pick which must stay up ("adopt what's running"). Refreshed
	// every few minutes; absent without systemd.
	Services []string `json:"services,omitempty"`

	// Fetches are status pages obs-beacond asked this agent to read on its own
	// host (HAProxy, NATS, Garage: they only listen on loopback). obs-beacond
	// interprets them, so the agent never needs to know their formats.
	Fetches []Fetch `json:"fetches,omitempty"`

	// Checks are the latest result of each check obs-beacond gave this agent.
	Checks []CheckResult `json:"checks,omitempty"`

	// Backups is the latest backup scan on this host (obs-beacon-agent scan,
	// run by root on a timer): what arrived, the backup jobs, the disks.
	Backups *Backups `json:"backups,omitempty"`

	// Map is where the host sits and what it talks to (docs/maps.md), from
	// the root helper; absent without it, or with `map: false`.
	Map *Map `json:"map,omitempty"`
	// Certs are the certificates the host's listening ports serve
	// (docs/cert-inventory.md, step 2): sent when they change and every
	// half hour; obs-beacond keeps the last.
	Certs *CertScan `json:"certs,omitempty"`
	// CertFiles is what the host's certificate files hold (step 3 of
	// docs/cert-inventory.md), sent like Certs.
	CertFiles *CertFiles `json:"cert_files,omitempty"`

	// Reloads are the units this agent may reload when they serve an old
	// certificate (agent.yaml cert_reload, docs/cert-rotation.md), so
	// obs-beacond offers only those.
	Reloads []ReloadRule `json:"reloads,omitempty"`
	// Actions are what came of actions obs-beacond handed this agent
	// (docs/cert-rotation.md), each sent once.
	Actions []ActionResult `json:"actions,omitempty"`

	// Sources are the names of the platforms and arrays this agent is set
	// to read now (Workloads.Source), so obs-beacond forgets one taken out
	// of its config. Always sent, [] for none; absent (null) from agents
	// older than it, whose sources are then kept.
	Sources []string `json:"sources"`
}

// Backups is one backup scan. obs-beacond judges it against backups.yaml.
type Backups struct {
	At       time.Time    `json:"at"`
	Received []BackupDir  `json:"received"` // backups other hosts sent here
	Made     []BackupDir  `json:"made"`     // backups any host may have made (the database dump)
	Roots    []BackupRoot `json:"roots"`    // the disks they're on
	Jobs     []BackupJob  `json:"jobs"`     // this host's backup jobs
	RAID     []RAIDArray  `json:"raid"`
	SMART    []DiskHealth `json:"smart"`
	Errors   []string     `json:"errors,omitempty"`
}

// BackupDir is the newest copy of one backup.
type BackupDir struct {
	Name     string    `json:"name"`           // e.g. mastodon-db
	From     string    `json:"from,omitempty"` // the sending host
	Path     string    `json:"path"`           // the newest copy's path
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitzero"`
	PrevSize int64     `json:"prev_size"` // the copy before it; 0 when there's none
	Count    int       `json:"count"`     // copies kept; 0: nothing has arrived
}

// BackupRoot is how full the disk under a backup directory is.
type BackupRoot struct {
	Path    string  `json:"path"`
	UsedPct float64 `json:"used_pct"`
}

// BackupJob is a backup service's last run.
type BackupJob struct {
	Unit     string    `json:"unit"`
	Result   string    `json:"result"` // success, exit-code, timeout...; empty: not found
	Active   string    `json:"active"`
	Finished time.Time `json:"finished,omitzero"`
}

// RAIDArray is one md array, as /proc/mdstat shows it: [UU] is healthy.
type RAIDArray struct {
	Dev   string `json:"dev"`
	State string `json:"state"`
}

// DiskHealth is smartctl's overall health verdict for one disk.
type DiskHealth struct {
	Disk  string `json:"disk"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"` // smartctl couldn't say
}

// Unit is one systemd unit's state, as systemctl shows it.
type Unit struct {
	Name     string    `json:"name"`           // e.g. patroni.service
	Load     string    `json:"load"`           // loaded, not-found, masked...
	Active   string    `json:"active"`         // active, inactive, failed, activating...
	Sub      string    `json:"sub"`            // running, dead, exited...
	Restarts int       `json:"restarts"`       // NRestarts since it was last started by hand
	Since    time.Time `json:"since,omitzero"` // when it last became active
}

// Fetch is one status page as the agent read it.
type Fetch struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Status int       `json:"status,omitempty"` // HTTP status; 0 when it couldn't be read
	Body   string    `json:"body,omitempty"`   // up to 256 KB
	Error  string    `json:"error,omitempty"`
}

// CheckResult is one check's latest run from this host.
type CheckResult struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Ms          float64   `json:"ms"`                    // how long it took (ping: average round trip)
	Status      int       `json:"status,omitempty"`      // HTTP status
	Answer      string    `json:"answer,omitempty"`      // DNS answer or banner, shortened
	CertExpires time.Time `json:"cert_expires,omitzero"` // TLS certificate seen, if any
	Cert        *Cert     `json:"cert,omitempty"`        // the certificate seen, described (from agents that send it)
	Loss        int       `json:"loss_pct,omitempty"`    // ping
}

// Cert is a certificate's public parts, as the inventory keeps them
// (docs/cert-inventory.md); internal/certinfo makes one.
type Cert struct {
	Fingerprint string    `json:"fingerprint"` // SHA-256 of the DER, hex
	Subject     string    `json:"subject"`     // the common name, or the first alternative name
	Names       []string  `json:"names,omitempty"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Key         string    `json:"key"` // "RSA 2048", "ECDSA P-256"
	Sig         string    `json:"sig"` // "SHA256-RSA"
	CA          bool      `json:"ca,omitempty"`
}

// CertScan is what a host's ports served, the last time each was tried.
type CertScan struct {
	At   time.Time  `json:"at"`
	List []CertSeen `json:"list"`
}

// CertSeen is one port's certificate.
type CertSeen struct {
	Addr  string    `json:"addr"` // dialed: 127.0.0.1 for every address
	Port  int       `json:"port"`
	Owner string    `json:"owner,omitempty"` // as the map sample names it
	How   string    `json:"how"`             // tls, or the STARTTLS protocol: smtp, imap, pop3, ldap, postgres
	Cert  Cert      `json:"cert"`
	Chain int       `json:"chain"` // intermediates sent with it
	At    time.Time `json:"at"`    // when it was tried
	// Wrong is what's wrong with how it's served (step 5): "doesn't
	// cover shop.example.org", "sent without its intermediate
	// certificate", "self-signed where a CA-signed certificate was".
	Wrong []string `json:"wrong,omitempty"`
}

// CertFiles is the certificates in a host's files, read through the root
// helper: only the certificates, never a key.
type CertFiles struct {
	At   time.Time  `json:"at"`
	List []CertFile `json:"list"`
}

// CertFile is one file's certificate: the first in it that isn't a CA's.
type CertFile struct {
	Path  string    `json:"path"`
	Mod   time.Time `json:"mod"` // the file's modification time
	Cert  Cert      `json:"cert"`
	Chain int       `json:"chain"` // more certificates in the file
}

// Mem is in bytes.
type Mem struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
	SwapTotal uint64 `json:"swap_total"`
	SwapFree  uint64 `json:"swap_free"`
}

// Disk is one mounted filesystem, in bytes.
type Disk struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	FS     string `json:"fs"`
	Size   uint64 `json:"size"`
	Used   uint64 `json:"used"`
	Avail  uint64 `json:"avail"`
}

// Net is the host's traffic since the previous report, in bytes per second,
// summed over its non-virtual interfaces.
type Net struct {
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
}

// Containers is what Docker reported. Error is set, and List empty, when
// the agent couldn't ask (no helper, Docker down).
type Containers struct {
	Error string      `json:"error,omitempty"`
	List  []Container `json:"list"`
}

// VMs are vm records, the shape a cluster's KubeVirt VMs take, with the
// reporting host as their source; Usage is what the running ones used
// since the previous report.
type VMs struct {
	Error string     `json:"error,omitempty"`
	List  []Workload `json:"list"`
	Usage []Usage    `json:"usage,omitempty"`
}

// PortMap is one published port: Public on the host to Private in the
// container.
type PortMap struct {
	Public  int    `json:"public"`
	Private int    `json:"private"`
	Proto   string `json:"proto"` // tcp or udp
}

// Container is one entry of Docker's container list.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Project string `json:"project,omitempty"` // compose project label
	State   string `json:"state"`             // running, exited, restarting, paused, created, dead
	Status  string `json:"status"`            // Docker's human text, e.g. "Up 3 hours (healthy)"
	Health  string `json:"health,omitempty"`  // healthy, unhealthy, starting, or empty when there is no check
	Created int64  `json:"created"`
	// Ports are its published ports, and IPs its addresses on Docker's
	// networks: how the maps tie a host port or a bridge address to it.
	Ports []PortMap `json:"ports,omitempty"`
	IPs   []string  `json:"ips,omitempty"`
	// Logs are the last lines of a container in trouble (restarting,
	// unhealthy, dead or recently exited), for the alert. Others send none.
	Logs []string `json:"logs,omitempty"`
}

// Action is something an admin asked an agent to do
// (GET /api/v1/agent/actions). The agent decides whether it may.
type Action struct {
	ID   string          `json:"id"`
	Kind string          `json:"kind"` // "reload"
	Args json.RawMessage `json:"args"`
}

// ActionResult is what came of one: done, or refused or failed and why.
type ActionResult struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ReloadRule is a unit an agent may reload, and the certificate files it
// serves (globs).
type ReloadRule struct {
	Unit  string   `json:"unit" yaml:"unit"`
	Files []string `json:"files" yaml:"files"`
}
