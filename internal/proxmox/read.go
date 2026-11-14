package proxmox

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// num takes a number as Proxmox sends it: a number, or now and then a
// string holding one.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return nil // something else: no number
	}
	*n = num(f)
	return nil
}

// resource is one row of /cluster/resources: a VM (qemu), a container
// (lxc), a node, or something we don't read.
type resource struct {
	ID       string `json:"id"`   // qemu/100, lxc/101, node/pve1
	Type     string `json:"type"` // qemu, lxc, node, storage, pool, sdn
	Node     string `json:"node"`
	Name     string `json:"name"`
	Status   string `json:"status"` // running, stopped; online, offline
	VMID     num    `json:"vmid"`
	Template num    `json:"template"`
	HAState  string `json:"hastate"`
	Lock     string `json:"lock"`
	Tags     string `json:"tags"`
	Pool     string `json:"pool"`
	MaxCPU   num    `json:"maxcpu"`
	CPU      num    `json:"cpu"` // a share of maxcpu, 0 … 1
	MaxMem   num    `json:"maxmem"`
	Mem      num    `json:"mem"`
	Uptime   num    `json:"uptime"`
}

// member is one row of /cluster/status: the cluster, or a node.
type member struct {
	Type    string `json:"type"` // cluster, node
	Name    string `json:"name"`
	Quorate num    `json:"quorate"`
	Nodes   num    `json:"nodes"`
	Online  num    `json:"online"`
}

// Snapshot is one read of the cluster.
type Snapshot struct {
	Cluster   string // the cluster's name, or the node's when it stands alone
	Clustered bool
	Quorate   bool
	Nodes     int // members, online or not
	Online    int
	Version   string
	Resources []resource
	UUIDs     map[int]string // VM id → its SMBIOS UUID, as read from its config
	Problems  map[string]string
	At        time.Time
}

// Reader reads a cluster again and again, keeping what changes rarely:
// the version, and each VM's SMBIOS UUID.
type Reader struct {
	c         *Client
	version   string
	versionAt time.Time
	uuids     map[int]uuidSeen
}

type uuidSeen struct {
	uuid string
	at   time.Time
}

// configsPerRead caps the VM configs read in one round, so a cluster of
// thousands is read in a few minutes, not all at once.
const configsPerRead = 50

// uuidAge is how long a VM's SMBIOS UUID is kept before it's read again.
const uuidAge = 30 * time.Minute

func NewReader(c *Client) *Reader { return &Reader{c: c, uuids: map[int]uuidSeen{}} }

// Read reads the cluster once. An error is the API not answering at all;
// anything less goes in Problems.
func (r *Reader) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Problems: map[string]string{}, At: now, UUIDs: map[int]string{}}
	var res []resource
	if err := r.c.get(ctx, "/cluster/resources", &res); err != nil {
		return nil, err
	}
	var members []member
	if err := r.c.get(ctx, "/cluster/status", &members); err != nil {
		s.Problems["cluster status"] = err.Error()
	}
	for _, m := range members {
		switch m.Type {
		case "cluster":
			s.Cluster, s.Clustered, s.Quorate, s.Nodes = m.Name, true, m.Quorate != 0, int(m.Nodes)
		case "node":
			if m.Online != 0 {
				s.Online++
			}
			if !s.Clustered && s.Cluster == "" {
				s.Cluster = m.Name
			}
		}
	}
	if !s.Clustered {
		s.Quorate, s.Nodes = true, max(1, s.Online)
	}
	if now.Sub(r.versionAt) > uuidAge || r.version == "" {
		var v struct {
			Version string `json:"version"`
		}
		if err := r.c.get(ctx, "/version", &v); err == nil {
			r.version, r.versionAt = v.Version, now
		}
	}
	s.Version = r.version
	read := 0
	seen := map[int]bool{}
	for _, x := range res {
		if x.Type != "qemu" || x.Template != 0 {
			continue
		}
		id := int(x.VMID)
		seen[id] = true
		u, ok := r.uuids[id]
		if (!ok || now.Sub(u.at) > uuidAge) && read < configsPerRead && x.Node != "" {
			read++
			var cfg struct {
				SMBIOS1 string `json:"smbios1"`
			}
			err := r.c.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", esc(x.Node), id), &cfg)
			if err != nil {
				s.Problems["VM configs"] = err.Error()
			} else {
				u = uuidSeen{uuid: smbiosUUID(cfg.SMBIOS1), at: now}
				r.uuids[id] = u
			}
		}
		if u.uuid != "" {
			s.UUIDs[id] = u.uuid
		}
	}
	for id := range r.uuids {
		if !seen[id] {
			delete(r.uuids, id)
		}
	}
	s.Resources = res
	return s, nil
}

// smbiosUUID is the uuid= in a VM's smbios1 setting
// ("uuid=5d307ca9-…,manufacturer=…"), lower case.
func smbiosUUID(s string) string {
	for _, part := range strings.Split(s, ",") {
		if u, ok := strings.CutPrefix(strings.TrimSpace(part), "uuid="); ok {
			return strings.ToLower(u)
		}
	}
	return ""
}
