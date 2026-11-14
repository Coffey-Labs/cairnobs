package clusterwatch

import (
	"encoding/json"
	"fmt"
	"time"
)

// The parts of each Kubernetes object the agent reads; json ignores the
// rest as it decodes, so a pod's spec or environment never sits in memory.

type meta struct {
	UID               string            `json:"uid"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Generation        int64             `json:"generation"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []ownerRef        `json:"ownerReferences"`
}

type ownerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller bool   `json:"controller"`
}

// controllerRef is the owner that controls the object, if any.
func (m meta) controllerRef() *ownerRef {
	for i, o := range m.OwnerReferences {
		if o.Controller {
			return &m.OwnerReferences[i]
		}
	}
	return nil
}

type condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

func findCond(cs []condition, t string) *condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

type namespace struct {
	Metadata meta `json:"metadata"`
}

type node struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Conditions  []condition       `json:"conditions"`
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			KubeletVersion string `json:"kubeletVersion"`
			OSImage        string `json:"osImage"`
			Architecture   string `json:"architecture"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type containerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *terminated `json:"terminated"`
}

type terminated struct {
	Reason   string `json:"reason"`
	ExitCode int    `json:"exitCode"`
}

// word is how it ended: "OOMKilled", or "exit 1" for a plain error.
func (t *terminated) word() string {
	if t.Reason == "" || t.Reason == "Error" {
		return fmt.Sprintf("exit %d", t.ExitCode)
	}
	return t.Reason
}

type containerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type pod struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Resources struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Message               string            `json:"message"`
		Conditions            []condition       `json:"conditions"`
		StartTime             *time.Time        `json:"startTime"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type deployment struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64       `json:"observedGeneration"`
		Replicas           int         `json:"replicas"`
		ReadyReplicas      int         `json:"readyReplicas"`
		UpdatedReplicas    int         `json:"updatedReplicas"`
		AvailableReplicas  int         `json:"availableReplicas"`
		Conditions         []condition `json:"conditions"`
	} `json:"status"`
}

type statefulSet struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64  `json:"observedGeneration"`
		ReadyReplicas      int    `json:"readyReplicas"`
		UpdatedReplicas    int    `json:"updatedReplicas"`
		CurrentRevision    string `json:"currentRevision"`
		UpdateRevision     string `json:"updateRevision"`
	} `json:"status"`
}

type daemonSet struct {
	Metadata meta `json:"metadata"`
	Status   struct {
		ObservedGeneration     int64 `json:"observedGeneration"`
		DesiredNumberScheduled int   `json:"desiredNumberScheduled"`
		NumberReady            int   `json:"numberReady"`
		UpdatedNumberScheduled int   `json:"updatedNumberScheduled"`
	} `json:"status"`
}

type replicaSet struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

type job struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Active         int         `json:"active"`
		Succeeded      int         `json:"succeeded"`
		Failed         int         `json:"failed"`
		StartTime      *time.Time  `json:"startTime"`
		CompletionTime *time.Time  `json:"completionTime"`
		Conditions     []condition `json:"conditions"`
	} `json:"status"`
}

type cronJob struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Schedule string `json:"schedule"`
		Suspend  *bool  `json:"suspend"`
	} `json:"spec"`
	Status struct {
		LastScheduleTime   *time.Time `json:"lastScheduleTime"`
		LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
	} `json:"status"`
}

type pvc struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		StorageClassName *string `json:"storageClassName"`
		VolumeName       string  `json:"volumeName"`
	} `json:"spec"`
	Status struct {
		Phase    string            `json:"phase"`
		Capacity map[string]string `json:"capacity"`
	} `json:"status"`
}

// OpenShift and OKD (config.openshift.io and machineconfiguration.openshift.io).

type clusterVersion struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Channel string `json:"channel"`
	} `json:"spec"`
	Status struct {
		Desired struct {
			Version string `json:"version"`
		} `json:"desired"`
		History []struct {
			State   string `json:"state"` // Completed or Partial
			Version string `json:"version"`
		} `json:"history"`
		Conditions []condition `json:"conditions"`
	} `json:"status"`
}

type clusterOperator struct {
	Metadata meta `json:"metadata"`
	Status   struct {
		Conditions []condition `json:"conditions"`
		Versions   []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"versions"`
	} `json:"status"`
}

type machineConfigPool struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Paused bool `json:"paused"`
	} `json:"spec"`
	Status struct {
		MachineCount         int         `json:"machineCount"`
		UpdatedMachineCount  int         `json:"updatedMachineCount"`
		ReadyMachineCount    int         `json:"readyMachineCount"`
		DegradedMachineCount int         `json:"degradedMachineCount"`
		Conditions           []condition `json:"conditions"`
	} `json:"status"`
}

// KubeVirt (kubevirt.io/v1) and CDI (cdi.kubevirt.io/v1beta1): served
// where OpenShift Virtualization or KubeVirt is installed.

type firmware struct {
	UUID string `json:"uuid"`
}

type virtualMachine struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Running     *bool  `json:"running"`
		RunStrategy string `json:"runStrategy"`
		Template    struct {
			Spec struct {
				Domain struct {
					Firmware firmware `json:"firmware"`
				} `json:"domain"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		PrintableStatus string      `json:"printableStatus"`
		Conditions      []condition `json:"conditions"`
	} `json:"status"`
}

// strategy is how the VM is meant to run: Always, RerunOnFailure, Manual,
// Halted, Once; the older spec.running says Always or Halted.
func (vm *virtualMachine) strategy() string {
	switch {
	case vm.Spec.RunStrategy != "":
		return vm.Spec.RunStrategy
	case vm.Spec.Running != nil && *vm.Spec.Running:
		return "Always"
	}
	return "Halted"
}

type vmiInterface struct {
	IPAddress   string   `json:"ipAddress"`
	IPAddresses []string `json:"ipAddresses"`
}

type virtualMachineInstance struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Domain struct {
			Firmware firmware `json:"firmware"`
		} `json:"domain"`
	} `json:"spec"`
	Status struct {
		Phase       string         `json:"phase"`
		Reason      string         `json:"reason"`
		NodeName    string         `json:"nodeName"`
		Conditions  []condition    `json:"conditions"`
		Interfaces  []vmiInterface `json:"interfaces"`
		GuestOSInfo struct {
			PrettyName string `json:"prettyName"`
		} `json:"guestOSInfo"`
		MigrationState *struct {
			TargetNode string     `json:"targetNode"`
			Completed  bool       `json:"completed"`
			Failed     bool       `json:"failed"`
			EndTime    *time.Time `json:"endTimestamp"`
		} `json:"migrationState"`
	} `json:"status"`
}

// agentConnected says whether the guest agent answers inside the VM.
func (v *virtualMachineInstance) agentConnected() bool {
	c := findCond(v.Status.Conditions, "AgentConnected")
	return c != nil && c.Status == "True"
}

type vmiMigration struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		VMIName string `json:"vmiName"`
	} `json:"spec"`
	Status struct {
		Phase      string      `json:"phase"`
		Conditions []condition `json:"conditions"`
		// When each phase began: the Failed one is when it failed.
		PhaseTransitionTimestamps []struct {
			Phase string    `json:"phase"`
			Time  time.Time `json:"phaseTransitionTimestamp"`
		} `json:"phaseTransitionTimestamps"`
	} `json:"status"`
}

type dataVolume struct {
	Metadata meta `json:"metadata"`
	Status   struct {
		Phase        string      `json:"phase"`
		Progress     string      `json:"progress"`
		RestartCount int         `json:"restartCount"`
		Conditions   []condition `json:"conditions"`
	} `json:"status"`
}

// guestFS is what the guest agent says about a VM's file systems, read
// every few minutes (guestfs.go), not watched.
type guestFS struct {
	Items []guestFSItem `json:"items"`
}

type guestFSItem struct {
	MountPoint     string `json:"mountPoint"`
	FileSystemType string `json:"fileSystemType"`
	UsedBytes      int64  `json:"usedBytes"`
	TotalBytes     int64  `json:"totalBytes"`
}

// For the maps (docs/maps.md, step 7): Services, the endpoints behind
// them, Ingresses and OpenShift Routes.

type servicePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	NodePort int    `json:"nodePort"`
}

type k8sService struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Type        string            `json:"type"`
		ClusterIPs  []string          `json:"clusterIPs"`
		ExternalIPs []string          `json:"externalIPs"`
		Selector    map[string]string `json:"selector"`
		Ports       []servicePort     `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type endpointSlice struct {
	Metadata  meta `json:"metadata"` // labels["kubernetes.io/service-name"]
	Endpoints []struct {
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
		TargetRef *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"targetRef"`
	} `json:"endpoints"`
}

type ingressBackend struct {
	Service *struct {
		Name string `json:"name"`
		Port struct {
			Number int    `json:"number"`
			Name   string `json:"name"`
		} `json:"port"`
	} `json:"service"`
}

type ingress struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		IngressClassName *string         `json:"ingressClassName"`
		DefaultBackend   *ingressBackend `json:"defaultBackend"`
		TLS              []ingressTLS    `json:"tls"`
		Rules            []struct {
			Host string `json:"host"`
			HTTP *struct {
				Paths []struct {
					Backend ingressBackend `json:"backend"`
				} `json:"paths"`
			} `json:"http"`
		} `json:"rules"`
	} `json:"spec"`
}

type route struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Host string `json:"host"`
		To   struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"to"`
		AlternateBackends []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"alternateBackends"`
		Port *struct {
			TargetPort any `json:"targetPort"`
		} `json:"port"`
		// Only the certificate is decoded: a Route with its certificate
		// inline carries its key too, which is never read.
		TLS *struct {
			Certificate string `json:"certificate"`
		} `json:"tls"`
	} `json:"spec"`
}

type ingressTLS struct {
	Hosts      []string `json:"hosts"`
	SecretName string   `json:"secretName"`
}

// resource is one kind the agent lists and watches.
type resource struct {
	name   string // for people: "pods"
	path   string // the collection: /api/v1/pods
	decode func(json.RawMessage) (uid string, obj any, err error)
}

func decoder[T any](uid func(*T) string) func(json.RawMessage) (string, any, error) {
	return func(raw json.RawMessage) (string, any, error) {
		v := new(T)
		if err := json.Unmarshal(raw, v); err != nil {
			return "", nil, err
		}
		return uid(v), v, nil
	}
}

// resources is everything this version watches (cluster-agent.yaml's
// ClusterRole grants these, and nothing else). A resource the cluster
// doesn't serve (404) is looked for again every 10 minutes.
var resources = []resource{
	{"namespaces", "/api/v1/namespaces", decoder(func(o *namespace) string { return o.Metadata.UID })},
	{"nodes", "/api/v1/nodes", decoder(func(o *node) string { return o.Metadata.UID })},
	{"pods", "/api/v1/pods", decoder(func(o *pod) string { return o.Metadata.UID })},
	{"deployments", "/apis/apps/v1/deployments", decoder(func(o *deployment) string { return o.Metadata.UID })},
	{"statefulsets", "/apis/apps/v1/statefulsets", decoder(func(o *statefulSet) string { return o.Metadata.UID })},
	{"daemonsets", "/apis/apps/v1/daemonsets", decoder(func(o *daemonSet) string { return o.Metadata.UID })},
	{"replicasets", "/apis/apps/v1/replicasets", decoder(func(o *replicaSet) string { return o.Metadata.UID })},
	{"jobs", "/apis/batch/v1/jobs", decoder(func(o *job) string { return o.Metadata.UID })},
	{"cronjobs", "/apis/batch/v1/cronjobs", decoder(func(o *cronJob) string { return o.Metadata.UID })},
	{"persistentvolumeclaims", "/api/v1/persistentvolumeclaims", decoder(func(o *pvc) string { return o.Metadata.UID })},
	// Served only by OpenShift and OKD; elsewhere they're 404 and skipped.
	{"clusterversions", "/apis/config.openshift.io/v1/clusterversions", decoder(func(o *clusterVersion) string { return o.Metadata.UID })},
	{"clusteroperators", "/apis/config.openshift.io/v1/clusteroperators", decoder(func(o *clusterOperator) string { return o.Metadata.UID })},
	{"machineconfigpools", "/apis/machineconfiguration.openshift.io/v1/machineconfigpools", decoder(func(o *machineConfigPool) string { return o.Metadata.UID })},
	// Served where KubeVirt (OpenShift Virtualization) and CDI are installed.
	{"virtualmachines", "/apis/kubevirt.io/v1/virtualmachines", decoder(func(o *virtualMachine) string { return o.Metadata.UID })},
	{"virtualmachineinstances", "/apis/kubevirt.io/v1/virtualmachineinstances", decoder(func(o *virtualMachineInstance) string { return o.Metadata.UID })},
	{"virtualmachineinstancemigrations", "/apis/kubevirt.io/v1/virtualmachineinstancemigrations", decoder(func(o *vmiMigration) string { return o.Metadata.UID })},
	{"datavolumes", "/apis/cdi.kubevirt.io/v1beta1/datavolumes", decoder(func(o *dataVolume) string { return o.Metadata.UID })},
	// For the maps: what reaches what.
	{"services", "/api/v1/services", decoder(func(o *k8sService) string { return o.Metadata.UID })},
	{"endpointslices", "/apis/discovery.k8s.io/v1/endpointslices", decoder(func(o *endpointSlice) string { return o.Metadata.UID })},
	{"ingresses", "/apis/networking.k8s.io/v1/ingresses", decoder(func(o *ingress) string { return o.Metadata.UID })},
	{"routes", "/apis/route.openshift.io/v1/routes", decoder(func(o *route) string { return o.Metadata.UID })},
	// Served where cert-manager is installed (docs/cert-inventory.md, step 4).
	{"certificates", "/apis/cert-manager.io/v1/certificates", decoder(func(o *cmCertificate) string { return o.Metadata.UID })},
}

// metrics.k8s.io/v1beta1 (metrics-server, or OpenShift's own): CPU and
// memory now, per node and per pod's container. Read every metricsEvery
// (metrics.go), not watched: the API doesn't serve watches.
type usageQ struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type nodeMetrics struct {
	Metadata meta   `json:"metadata"`
	Usage    usageQ `json:"usage"`
}

type podMetrics struct {
	Metadata   meta `json:"metadata"`
	Containers []struct {
		Usage usageQ `json:"usage"`
	} `json:"containers"`
}

type metricsList[T any] struct {
	Items []T `json:"items"`
}
