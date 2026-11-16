package server

import (
	"cmp"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Platforms (#80): what a cluster, array or VM host is, said the way people
// name it, so the pages can show OpenShift apart from plain Kubernetes and
// VMware apart from Nutanix or Proxmox. A cluster's comes from the
// distribution its watcher reports; a host's VMs' from their hypervisor.

// platformOf names the platform from a cluster's distribution, or for a
// host's own VMs (distribution ""), from their hypervisor fact.
func platformOf(distribution, hypervisor string) string {
	switch d := distribution; {
	case d == "OpenShift", d == "OKD":
		return d
	case d == "vCenter":
		return "VMware vSphere"
	case d == "Prism Central":
		return "Nutanix"
	case strings.HasPrefix(d, "Proxmox"):
		return "Proxmox VE"
	case d == "Purity//FB":
		return "Pure FlashBlade"
	case strings.HasPrefix(d, "Purity"):
		return "Pure FlashArray"
	case d == ibmiDistribution:
		return "IBM i"
	case d == hmcDistribution:
		return "IBM Power"
	case d == "OpenBao":
		return d
	case strings.HasPrefix(d, "Vault"):
		return "Vault"
	case d != "":
		return "Kubernetes"
	}
	switch hypervisor {
	case "Hyper-V":
		return "Hyper-V"
	case "libvirt":
		return "KVM"
	case "vSphere":
		return "VMware vSphere"
	}
	return ""
}

// platform is the source's: a cluster's from its distribution (a cluster
// agent that hasn't said is Kubernetes), a host's from its VMs.
func (s wsnap) platform() string {
	if s.host {
		for _, r := range s.recs {
			if h, _ := r.Facts["hypervisor"].(string); h != "" {
				return platformOf("", h)
			}
		}
		return "KVM" // a host lists VMs through libvirt unless it says otherwise
	}
	if p := platformOf(s.distribution(), ""); p != "" {
		return p
	}
	return "Kubernetes"
}

// clusterPlatform is a stored cluster source's platform, from its own
// record (vmLinks reads the store directly, under its lock).
func clusterPlatform(recs map[string]*wrec) string {
	d := distributionOf(func(yield func(report.Workload) bool) {
		for _, r := range recs {
			if !yield(r.Workload) {
				return
			}
		}
	})
	return cmp.Or(platformOf(d, ""), "Kubernetes")
}
