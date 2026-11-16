package server

import (
	"cmp"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A VM that also runs our agent is one machine seen from two sides
// (docs/cluster-watcher.md, Linking a VM to its agent): the agent reports
// the SMBIOS UUID, the hypervisor the VM's firmware UUID, and they're the
// same number. Linked, the pages show both, and a VM that's down, or on a
// node that's down, explains its host going silent.

// VMLink is the VM a host is, as the hypervisor sees it.
type VMLink struct {
	Source    string `json:"source"` // the cluster, or the libvirt host it runs on (later, a Proxmox cluster)
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	State     string `json:"state"`
	Word      string `json:"word"`
	Href      string `json:"href"`
	OnHost    bool   `json:"on_host,omitempty"`  // Source is a host running libvirt, not a cluster
	Title     string `json:"title,omitempty"`    // the cluster's own name, when it isn't Source's (Proxmox)
	Platform  string `json:"platform,omitempty"` // what Source runs on: VMware vSphere, OpenShift, KVM (#80)
	key       string // its condition's key: vm/<source>/<ns>/<name>
}

// HostLink is the host a VM is, as its agent sees it.
type HostLink struct {
	Name string `json:"name"`
	Href string `json:"href"`
}

// vmLinks matches hosts' SMBIOS UUIDs with VMs' firmware UUIDs. A UUID
// claimed by more than one host, or more than one VM (a clone that kept
// its UUID), links nothing: a wrong link would hide a real outage.
func (srv *Server) vmLinks() (byHost map[string]VMLink, byVM map[string]HostLink) {
	hostOf := map[string]string{}
	dupHost := map[string]bool{}
	for name, m := range srv.store.Machines() {
		u := report.MachineUUID(m)
		if u == "" {
			continue
		}
		if _, ok := hostOf[u]; ok {
			dupHost[u] = true
		}
		hostOf[u] = name
	}
	if len(hostOf) == 0 {
		return nil, nil
	}
	vmOf := map[string]VMLink{}
	dupVM := map[string]bool{}
	ws := &srv.wl
	ws.mu.RLock()
	for src, s := range ws.src {
		title, plat := "", clusterPlatform(s.recs)
		if r, ok := s.recs[report.ClusterSelf]; ok && r.Name != "" && r.Name != src {
			title = r.Name
		}
		for _, r := range s.recs {
			if r.Kind != report.KindVM {
				continue
			}
			f, _ := r.Facts["firmware_uuid"].(string)
			u := report.MachineUUID(f)
			if hostOf[u] == "" && r.Facts["hypervisor"] == "vSphere" {
				u = report.SwappedUUID(u) // older VMware hardware shows the guest its first fields byte-swapped
			}
			if u == "" || hostOf[u] == "" {
				continue
			}
			if _, ok := vmOf[u]; ok {
				dupVM[u] = true
			}
			vmOf[u] = VMLink{Source: src, Title: title, Platform: plat, Namespace: r.Namespace, Name: r.Name, Node: r.Node, State: r.State, Word: r.Word,
				Href: workloadHref(src, r.Namespace, r.Name), key: "vm/" + src + "/" + r.Namespace + "/" + r.Name}
		}
	}
	ws.mu.RUnlock()
	for _, h := range srv.store.HostVMs() {
		for _, r := range h.vms.List {
			f, _ := r.Facts["firmware_uuid"].(string)
			u := report.MachineUUID(f)
			if r.Kind != report.KindVM || u == "" || hostOf[u] == "" {
				continue
			}
			if _, ok := vmOf[u]; ok {
				dupVM[u] = true
			}
			hv, _ := r.Facts["hypervisor"].(string)
			vmOf[u] = VMLink{Source: h.name, Platform: cmp.Or(platformOf("", hv), "KVM"), Name: r.Name, State: r.State, Word: r.Word,
				Href: workloadHref(h.name, "", r.Name), OnHost: true, key: "vm/" + h.name + "//" + r.Name}
		}
	}
	byHost, byVM = map[string]VMLink{}, map[string]HostLink{}
	for u, v := range vmOf {
		if dupHost[u] || dupVM[u] {
			continue
		}
		h := hostOf[u]
		byHost[h] = v
		byVM[v.key] = HostLink{Name: h, Href: "/hosts/" + h}
	}
	return byHost, byVM
}

// vmRunning says whether the hypervisor has the VM up (migrating counts).
func vmRunning(state string) bool {
	return state == report.StateOK || state == report.StateUpdating || state == report.StateDegraded
}

// explainSilent tells a silent host's condition what its VM is doing, so
// "gone silent" says "its VM is stopped" when that's why.
func explainSilent(conds []Condition, byHost map[string]VMLink) {
	if len(byHost) == 0 {
		return
	}
	for i, c := range conds {
		host, ok := strings.CutPrefix(c.Key, "silent/")
		if !ok {
			continue
		}
		v, ok := byHost[host]
		if !ok {
			continue
		}
		vm := "VM " + where(v.Name, v.Namespace) + " (" + v.place() + ")"
		if vmRunning(v.State) {
			conds[i].Detail = strings.TrimSpace(c.Detail + " It's " + vm + ", which is running as far as the hypervisor knows: look at the guest, its agent or the network.")
		} else {
			conds[i].Detail = strings.TrimSpace(c.Detail + " It's " + vm + ", which is " + lowerFirst(v.Word) + ".")
		}
	}
}

// place names where the VM runs: the cluster's own name, else the source's.
func (v VMLink) place() string {
	if v.Title != "" {
		return v.Title
	}
	return v.Source
}
