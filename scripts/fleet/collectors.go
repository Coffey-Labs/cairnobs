package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Host names (#40): a site's hosts are named for where they are and what
// they do, "phx1-app07", "iad1-db02", "lon1-kvm01", as a real fleet's
// are, and the agents that read storage, IBM i and virtualization are
// small hosts of their own, not app servers doing it on the side.

// siteCodes are the sites in turn; the fifth is phx2, and so on. Their
// time zones are siteZone's (load.go).
var siteCodes = []string{"phx", "iad", "lon", "fra"}

func siteCode(site int) string {
	return fmt.Sprintf("%s%d", siteCodes[site%len(siteCodes)], site/len(siteCodes)+1)
}

// label is what a host's name says it does: its role in the site
// (role), or kvm for a libvirt host.
func label(i int) string {
	if isHypervisor(i) {
		return "kvm"
	}
	return map[string]string{"gateway": "gw", "lb": "lb", "app": "app", "db": "db", "cache": "cache", "mq": "mq"}[role(i%siteSize)]
}

// hostName is fleet host i's name: its site, its label and its number
// among the site's hosts with that label.
func hostName(i int) string {
	site, pos := i/siteSize, i%siteSize
	l, n := label(i), 0
	for p := 0; p <= pos; p++ {
		if label(site*siteSize+p) == l {
			n++
		}
	}
	return fmt.Sprintf("%s-%s%02d", siteCode(site), l, n)
}

var (
	indexMu sync.Mutex
	indexOf = map[string]int{}
)

// hostIndex is the fleet host a name is, or -1.
func hostIndex(name string) int {
	indexMu.Lock()
	defer indexMu.Unlock()
	if i, ok := indexOf[name]; ok {
		return i
	}
	i := findHost(name)
	indexOf[name] = i
	return i
}

func findHost(name string) int {
	code, _, ok := strings.Cut(name, "-")
	if !ok {
		return -1
	}
	site := -1
	for s := range 400 {
		if siteCode(s) == code {
			site = s
			break
		}
	}
	if site < 0 {
		return -1
	}
	for p := range siteSize {
		if i := site*siteSize + p; hostName(i) == name {
			return i
		}
	}
	return -1
}

// collector is an agent on a host of its own that reads things with an
// API instead of an agent: arrays, IBM i, hypervisor managers.
type collector struct {
	name    string
	sources func() []string // what its config lists, read or not, as the agent says
}

var (
	storageCollector = collector{"phx1-stor01", func() []string { return pureArrays }}
	ibmiCollector    = collector{"phx1-ibmi01", func() []string { return append(append([]string{}, ibmiPartitions...), hmcName) }}
	virtCollector    = collector{"phx1-virt01", func() []string { return []string{"proxmox", vcenterName, prismCentral} }}
	collectors       = []collector{storageCollector, ibmiCollector, virtCollector}
)

func collectorFor(name string) (collector, bool) {
	for _, c := range collectors {
		if c.name == name {
			return c, true
		}
	}
	return collector{}, false
}

// newCollectorHost is a collector's own host: a small RHEL 9 VM that does
// nothing else.
func newCollectorHost(c collector, interval time.Duration) *host {
	d := rhel9
	h := &host{start: time.Now().Add(-time.Duration(20+len(c.name)) * 24 * time.Hour)}
	h.r = report.Report{
		V: report.Version, Agent: "fleet", Host: c.name, Interval: int(interval / time.Second),
		Kernel: d.kernel, OS: d.os, CPUs: 4,
		Mem: report.Mem{Total: 8 << 30, SwapTotal: 2 << 30, SwapFree: 2 << 30},
		Disks: []report.Disk{
			{Mount: "/", Device: d.rootDev, FS: d.rootFS, Size: 60 << 30},
			{Mount: "/var/lib/cairn-obs", Device: "/dev/vdb", FS: "xfs", Size: 40 << 30},
		},
		Sources:    c.sources(),
		Containers: &report.Containers{List: []report.Container{}},
	}
	return h
}

// isHypervisor says host i runs libvirt: host 1 in every 20 when that's
// an app server's place in its site (a load balancer or a database
// doesn't host VMs on the side).
func isHypervisor(i int) bool { return i%hypervisorEvery == 1 && role(i%siteSize) == "app" }

func isCollector(name string) bool { _, ok := collectorFor(name); return ok }
