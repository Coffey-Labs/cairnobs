package report

import "time"

// Workloads is one batch from a cluster agent: the nodes, pods, VMs and
// volumes it watches, as short records (docs/cluster-watcher.md, The
// record). The agent sends everything when it starts and every 30 minutes
// (Full "begin", "page"..., "end", all with one Generation), and changes in
// between (Full ""), at least every 30 seconds even when nothing changed.
type Workloads struct {
	V          int        `json:"v"`
	Agent      string     `json:"agent"`      // agent build version
	Generation string     `json:"generation"` // the latest full listing's id
	Full       string     `json:"full,omitempty"`
	Records    []Workload `json:"records"`
	Removed    []string   `json:"removed,omitempty"` // ids gone since the last batch
	// Source names what the batch is about when a host agent reads
	// something beside its host: a Proxmox cluster, a vCenter, Prism
	// Central, an array. Each is a source of its own, kept as
	// SourceKey(agent, Source). Empty: the agent's own cluster.
	Source string `json:"source,omitempty"`
	// Usage is what nodes, controllers and VMs used at UsageAt, from
	// metrics.k8s.io: sent once after each reading (every minute), not
	// kept in the records, so it doesn't rewrite them.
	Usage   []Usage   `json:"usage,omitempty"`
	UsageAt time.Time `json:"usage_at,omitzero"`
	// Actions are what came of actions obs-beacond handed a cluster agent
	// (docs/cert-rotation.md, step 3), each sent once, as a host agent's
	// report does.
	Actions []ActionResult `json:"actions,omitempty"`
}

// Usage is one record's CPU and memory now. For a controller it's the sum
// of its pods; for a VM, its launcher pod. CPUOf and MemOf are what it may
// use: a node's allocatable, the sum of the pods' limits (0 when any pod
// has none, so there's no ceiling).
type Usage struct {
	ID    string  `json:"id"`
	CPU   float64 `json:"cpu"` // cores
	Mem   int64   `json:"mem"` // bytes
	CPUOf float64 `json:"cpu_of,omitempty"`
	MemOf int64   `json:"mem_of,omitempty"`
	Pods  int     `json:"pods,omitempty"` // how many pods are summed
	// IO is an array's performance and space (docs/pure.md, step 4).
	IO *IO `json:"io,omitempty"`
	// Leases is a Vault cluster's lease count (docs/vault.md, step 3).
	Leases *int64 `json:"leases,omitempty"`
}

// IO is an array's reads and writes now, as Purity averages them over
// its last few seconds, and the space it uses.
type IO struct {
	ReadOps  float64 `json:"read_ops"`  // a second
	WriteOps float64 `json:"write_ops"` // a second
	ReadUs   float64 `json:"read_us"`   // microseconds a read takes
	WriteUs  float64 `json:"write_us"`  // microseconds a write takes
	ReadBps  float64 `json:"read_bps"`  // bytes a second
	WriteBps float64 `json:"write_bps"` // bytes a second
	Used     int64   `json:"used"`      // bytes
	UsedOf   int64   `json:"used_of,omitempty"`
}

// SourceKey is what obs-beacond keeps a source's records under: the
// agent's name, and the source's after a colon when it has one
// ("mgmt1:vcsa-01"). Agent names never hold a colon.
func SourceKey(agent, source string) string {
	if source == "" {
		return agent
	}
	return agent + ":" + source
}

// The parts of a full listing.
const (
	FullBegin = "begin"
	FullPage  = "page"
	FullEnd   = "end"
)

// Workload is one thing a cluster agent watches.
type Workload struct {
	Kind      string            `json:"kind"` // see the Kind constants
	ID        string            `json:"id"`   // stable for the object's life (a Kubernetes uid)
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Owner     string            `json:"owner,omitempty"` // "deployment/web": what the pod belongs to
	Node      string            `json:"node,omitempty"`
	State     string            `json:"state"` // see the State constants
	Word      string            `json:"word"`  // the state for people: "Restarting (CrashLoopBackOff)"
	Since     time.Time         `json:"since,omitzero"`
	Alert     string            `json:"alert,omitempty"` // page, warn or off, from obs.cairnobs.org/alert; "" is the default
	Labels    map[string]string `json:"labels,omitempty"`
	Facts     map[string]any    `json:"facts,omitempty"` // small, kind-specific: restarts, ready "2/3", version
}

// Kinds.
const (
	KindCluster    = "cluster" // the cluster itself: whether its API answers, its version
	KindNode       = "node"
	KindPod        = "pod"
	KindController = "controller" // a Deployment, StatefulSet, DaemonSet or ReplicaSet; Name is "deployment/web"
	KindJob        = "job"
	KindPVC        = "pvc"
	KindVM         = "vm"
	// A certificate: Name its subject, Namespace where it's from (a PKI
	// mount), judged by the time it has left (docs/vault.md).
	KindCert = "cert"
	// What the maps draw edges from (docs/maps.md, step 7): a Service, with
	// the controllers behind it, and an Ingress or OpenShift Route, with
	// the Services it sends to. Not judged.
	KindService = "service"
	KindIngress = "ingress" // Name is "ingress/web" or "route/web"
)

// ClusterSelf is the ID of a cluster's own record: whether its API answers,
// its version. Other cluster records are its parts, named by what they
// are: "clusterversion/version", "clusteroperator/ingress",
// "machineconfigpool/worker" (OpenShift and OKD).
const ClusterSelf = "cluster"

// States, the same for every kind and source.
const (
	StateOK        = "ok"
	StateStarting  = "starting"
	StateStopped   = "stopped" // on purpose: a VM halted, a controller scaled to 0
	StateDegraded  = "degraded"
	StateDown      = "down"
	StateCrashloop = "crashloop"
	StatePending   = "pending"
	StateUpdating  = "updating" // a node being updated: cordoned, drained, rebooted
	StateUnknown   = "unknown"
)

// Alert settings from the obs.cairnobs.org/alert annotation.
const (
	AlertPage = "page"
	AlertWarn = "warn"
	AlertOff  = "off"
)
