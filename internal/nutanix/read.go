package nutanix

import (
	"context"
	"fmt"
	"time"
)

// The API versions read: v4.0 is the first GA, and every Prism Central
// since pc.2024.1 answers it.
const (
	vmsPath        = "/vmm/v4.0/ahv/config/vms"
	clustersPath   = "/clustermgmt/v4.0/config/clusters"
	hostsPath      = "/clustermgmt/v4.0/config/hosts"
	containersPath = "/clustermgmt/v4.0/config/storage-containers"
	alertsPath     = "/monitoring/v4.0/serviceability/alerts"
)

type ref struct {
	ExtID string `json:"extId"`
}

// Cluster is one of /clusters: a Prism Element cluster, or Prism Central
// itself (clusterFunction PRISM_CENTRAL).
type Cluster struct {
	ExtID  string `json:"extId"`
	Name   string `json:"name"`
	Config struct {
		BuildInfo struct {
			Version string `json:"version"`
		} `json:"buildInfo"`
		ClusterFunction []string `json:"clusterFunction"`
		IsAvailable     *bool    `json:"isAvailable"`
		OperationMode   string   `json:"operationMode"`
	} `json:"config"`
	VMCount int64 `json:"vmCount"`
}

func (c Cluster) isPrismCentral() bool {
	for _, f := range c.Config.ClusterFunction {
		if f == "PRISM_CENTRAL" {
			return true
		}
	}
	return false
}

// Host is one of /hosts: a node of a cluster.
type Host struct {
	ExtID    string `json:"extId"`
	HostName string `json:"hostName"`
	HostType string `json:"hostType"`
	Cluster  struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"cluster"`
	BlockModel       string `json:"blockModel"`
	NumberOfCores    int64  `json:"numberOfCpuCores"`
	MemorySizeBytes  int64  `json:"memorySizeBytes"`
	IsDegraded       bool   `json:"isDegraded"`
	NodeStatus       string `json:"nodeStatus"`
	MaintenanceState string `json:"maintenanceState"`
	Hypervisor       struct {
		FullName                 string `json:"fullName"`
		Type                     string `json:"type"`
		State                    string `json:"state"`
		AcropolisConnectionState string `json:"acropolisConnectionState"`
		NumberOfVMs              int64  `json:"numberOfVms"`
	} `json:"hypervisor"`
}

// VM is one of /vms: an AHV VM.
type VM struct {
	ExtID             string `json:"extId"`
	Name              string `json:"name"`
	BiosUUID          string `json:"biosUuid"`
	PowerState        string `json:"powerState"`
	Cluster           *ref   `json:"cluster"`
	Host              *ref   `json:"host"`
	NumSockets        int    `json:"numSockets"`
	NumCoresPerSocket int    `json:"numCoresPerSocket"`
	NumThreadsPerCore int    `json:"numThreadsPerCore"`
	MemorySizeBytes   int64  `json:"memorySizeBytes"`
	IsAgentVM         bool   `json:"isAgentVm"`
	GuestTools        *struct {
		IsInstalled    bool   `json:"isInstalled"`
		IsReachable    bool   `json:"isReachable"`
		GuestOSVersion string `json:"guestOsVersion"`
	} `json:"guestTools"`
	Categories []ref `json:"categories"`
}

// Container is one of /storage-containers.
type Container struct {
	ExtID              string `json:"extId"`
	Name               string `json:"name"`
	ClusterExtID       string `json:"clusterExtId"`
	ClusterName        string `json:"clusterName"`
	MaxCapacityBytes   int64  `json:"maxCapacityBytes"`
	ReplicationFactor  int    `json:"replicationFactor"`
	IsInternal         bool   `json:"isInternal"`
	IsMarkedForRemoval bool   `json:"isMarkedForRemoval"`
}

// Alert is one of /alerts, unresolved.
type Alert struct {
	ExtID          string `json:"extId"`
	Title          string `json:"title"`
	Severity       string `json:"severity"` // INFO, WARNING, CRITICAL
	IsAcknowledged bool   `json:"isAcknowledged"`
	SourceEntity   *struct {
		ExtID string `json:"extId"`
		Name  string `json:"name"`
		Type  string `json:"type"`
	} `json:"sourceEntity"`
	AffectedEntities []struct {
		ExtID string `json:"extId"`
		Name  string `json:"name"`
		Type  string `json:"type"`
	} `json:"affectedEntities"`
}

// Snapshot is one read of Prism Central.
type Snapshot struct {
	Name       string // Prism Central's host name
	Clusters   []Cluster
	Hosts      []Host
	VMs        []VM
	Containers []Container
	Alerts     []Alert
	Problems   map[string]string
	At         time.Time
}

// Read reads everything once. The VMs are what it's for: without them
// the read failed; anything else that can't be read is a problem on
// Prism Central's own record.
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Name: c.name, Problems: map[string]string{}, At: now}
	if err := list(ctx, c, vmsPath, "", &s.VMs); err != nil {
		return nil, fmt.Errorf("VMs: %w", err)
	}
	if err := list(ctx, c, clustersPath, "", &s.Clusters); err != nil {
		s.Problems["clusters"] = err.Error()
	}
	if err := list(ctx, c, hostsPath, "", &s.Hosts); err != nil {
		s.Problems["hosts"] = err.Error()
	}
	if err := list(ctx, c, containersPath, "", &s.Containers); err != nil {
		s.Problems["storage containers"] = err.Error()
	}
	if err := list(ctx, c, alertsPath, "isResolved eq false", &s.Alerts); err != nil {
		s.Problems["alerts"] = err.Error()
	}
	return s, nil
}
