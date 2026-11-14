package libvirt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Sockets are where libvirt's read-only socket is, in the order tried: the
// monolithic libvirtd (or virtproxyd, standing in for it), then the
// modular virtqemud on its own.
var Sockets = []string{"/run/libvirt/libvirt-sock-ro", "/run/libvirt/virtqemud-sock-ro"}

// ErrAbsent is a host without libvirt: no VMs, and nothing wrong.
var ErrAbsent = errors.New("libvirt isn't running here")

// Domain is one VM as the helper hands it to the agent: libvirt's own
// numbers, read once. The agent makes records of them.
type Domain struct {
	Name      string `json:"name"`
	UUID      string `json:"uuid"`
	State     int    `json:"state"`  // virDomainState: 1 running … 5 shut off, 6 crashed
	Reason    int    `json:"reason"` // the state's reason, as libvirt numbers it
	Autostart bool   `json:"autostart"`
	VCPUs     int    `json:"vcpus"`
	MemKiB    uint64 `json:"mem_kib"`      // what it has now (balloon.current)
	MaxKiB    uint64 `json:"max_kib"`      // the most it can have (balloon.maximum)
	RSSKiB    uint64 `json:"rss_kib"`      // what its QEMU process holds on the host; 0 when off
	CPUNs     uint64 `json:"cpu_time_ns"`  // CPU time used since it started
	Read      int64  `json:"read_unix_ms"` // when these were read
}

// Stats groups asked for: state, CPU total, balloon, vCPUs.
const statGroups = 1 | 2 | 4 | 8

// Read reads every domain from the first socket that's there, as uri
// (qemu:///system on a host).
func Read(sockets []string, uri string, timeout time.Duration) ([]Domain, error) {
	var socket string
	for _, s := range sockets {
		if _, err := os.Stat(s); err == nil {
			socket = s
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if socket == "" {
		return nil, ErrAbsent
	}
	c, err := dial(socket, timeout)
	if err != nil {
		return nil, fmt.Errorf("libvirt at %s: %w", socket, err)
	}
	defer c.close()
	if err := c.open(uri); err != nil {
		return nil, fmt.Errorf("libvirt at %s: %w", socket, err)
	}
	defer c.call(procConnectClose, nil) //nolint:errcheck // closing politely; the socket closes anyway
	recs, err := c.stats(statGroups)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	out := make([]Domain, 0, len(recs))
	for _, r := range recs {
		d := Domain{Name: r.dom.name, UUID: uuidString(r.dom.uuid), Read: now,
			State: int(num(r.params["state.state"])), Reason: int(num(r.params["state.reason"])),
			VCPUs: int(num(r.params["vcpu.current"])), MemKiB: unum(r.params["balloon.current"]),
			MaxKiB: unum(r.params["balloon.maximum"]), RSSKiB: unum(r.params["balloon.rss"]), CPUNs: unum(r.params["cpu.time"])}
		if a, err := c.autostart(r.dom); err == nil {
			d.Autostart = a
		}
		out = append(out, d)
	}
	return out, nil
}

func num(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func unum(v any) uint64 {
	if n := num(v); n > 0 {
		return uint64(n)
	}
	return 0
}
