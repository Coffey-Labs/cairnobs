// Package flashblade reads a Pure Storage FlashBlade through Purity//FB's
// REST API 2 (docs/pure.md, step 5), as a user holding the built-in
// readonly role, with an API token: the array, its blades, hardware, file
// systems, buckets, their replica links, replication connections, open
// alerts, space and performance. Only GET is asked, after logging in.
package flashblade

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/cairnobs/cairnobs/internal/purerest"
)

// Config is one array in the agent's pure: section, with type: flashblade.
type Config = purerest.Config

// Client asks one FlashBlade, read only.
type Client struct{ *purerest.Client }

// New checks cfg and reads the token and the CA; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	cfg.Type = purerest.FlashBlade
	c, err := purerest.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

type ref struct {
	Name string `json:"name"`
}

// Space is how the array, a file system or a bucket uses its flash.
// TotalPhysical is older Purity//FB's word for TotalUsed.
type Space struct {
	DataReduction float64 `json:"data_reduction"`
	TotalPhysical int64   `json:"total_physical"`
	TotalUsed     int64   `json:"total_used"`
	Unique        int64   `json:"unique"`
	Snapshots     int64   `json:"snapshots"`
	Virtual       int64   `json:"virtual"`
}

// Used is the space used: total_used, or total_physical on older ones.
func (s Space) Used() int64 {
	if s.TotalUsed > 0 {
		return s.TotalUsed
	}
	return s.TotalPhysical
}

// Array is /arrays' one item.
type Array struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OS          string `json:"os"` // Purity//FB
	Version     string `json:"version"`
	ProductType string `json:"product_type"` // FlashBlade, FlashBlade//S, FlashBlade//E
}

// ArraySpace is /arrays/space's one item.
type ArraySpace struct {
	Capacity int64    `json:"capacity"`
	Parity   *float64 `json:"parity"` // below 1.0 while redundancy is rebuilt
	Space    Space    `json:"space"`
}

// Blade is one of /blades.
type Blade struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"` // CH1.FB1
	Status      string  `json:"status"`
	Details     string  `json:"details"`
	Progress    float64 `json:"progress"`
	RawCapacity int64   `json:"raw_capacity"`
}

// Part is one of /hardware: only what isn't healthy is kept, and not the
// blades (they're their own records).
type Part struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details"`
	Type    string `json:"type"`
}

// FileSystem is one of /file-systems, not destroyed.
type FileSystem struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Created          int64  `json:"created"`
	Provisioned      int64  `json:"provisioned"`
	HardLimitEnabled bool   `json:"hard_limit_enabled"`
	PromotionStatus  string `json:"promotion_status"`
	Writable         *bool  `json:"writable"`
	Space            Space  `json:"space"`
	NFS              *struct {
		V3  bool `json:"v3_enabled"`
		V41 bool `json:"v4_1_enabled"`
	} `json:"nfs"`
	SMB *struct {
		Enabled bool `json:"enabled"`
	} `json:"smb"`
	HTTP *struct {
		Enabled bool `json:"enabled"`
	} `json:"http"`
}

// Bucket is one of /buckets, not destroyed.
type Bucket struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Account     *ref   `json:"account"`
	Created     int64  `json:"created"`
	ObjectCount int64  `json:"object_count"`
	QuotaLimit  *int64 `json:"quota_limit"`
	Versioning  string `json:"versioning"`
	Space       Space  `json:"space"`
}

// Link is one of /file-system-replica-links or /bucket-replica-links:
// replication of a file system or a bucket, to or from another array.
type Link struct {
	ID            string `json:"id"`
	Direction     string `json:"direction"` // inbound, outbound
	Status        string `json:"status"`    // replicating, idle, quiescing, quiesced, paused, unhealthy
	StatusDetails string `json:"status_details"`
	Lag           *int64 `json:"lag"` // ms
	Paused        bool   `json:"paused"`
	Remote        *ref   `json:"remote"`
	LocalFS       *ref   `json:"local_file_system"`
	LocalBucket   *ref   `json:"local_bucket"`
}

// Connection is one of /array-connections: replication to another array.
type Connection struct {
	ID      string `json:"id"`
	Remote  ref    `json:"remote"`
	Status  string `json:"status"` // connected, partially_connected, connecting, incompatible
	Type    string `json:"type"`   // async-replication, fleet-management
	Version string `json:"version"`
}

// Snapshot is one read of an array.
type Snapshot struct {
	Name        string // what it's kept under
	Array       Array
	Space       *ArraySpace
	Blades      []Blade
	Unhealthy   []Part
	FileSystems []FileSystem
	Buckets     []Bucket
	Links       []Link
	Connections []Connection
	Alerts      []purerest.Alert
	Perf        *purerest.Performance // nil when it couldn't be read: PerfErr says why
	PerfErr     string
	Problems    map[string]string
	At          time.Time
}

// healthy are the hardware states that aren't trouble.
var healthy = map[string]bool{"healthy": true, "unused": true, "identifying": true, "": true}

// Read reads the array once. The array itself is what it's for: without
// it the read failed; anything else that can't be read is a problem on
// the array's own record.
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Name: c.Name(), Problems: map[string]string{}, At: now}
	var arrays []Array
	if err := purerest.List(ctx, c.Client, "/arrays", nil, &arrays); err != nil {
		return nil, err
	}
	if len(arrays) == 0 {
		return nil, fmt.Errorf("flashblade: /arrays listed nothing")
	}
	s.Array = arrays[0]
	var space []ArraySpace
	if err := purerest.List(ctx, c.Client, "/arrays/space", nil, &space); err != nil {
		s.Problems["space"] = err.Error()
	} else if len(space) > 0 {
		s.Space = &space[0]
	}
	if err := purerest.List(ctx, c.Client, "/blades", nil, &s.Blades); err != nil {
		s.Problems["blades"] = err.Error()
	}
	var parts []Part
	if err := purerest.List(ctx, c.Client, "/hardware", nil, &parts); err != nil {
		s.Problems["hardware"] = err.Error()
	}
	for _, p := range parts {
		if !healthy[p.Status] && p.Type != "fb" {
			s.Unhealthy = append(s.Unhealthy, p)
		}
	}
	notDestroyed := url.Values{"destroyed": {"false"}}
	if err := purerest.List(ctx, c.Client, "/file-systems", notDestroyed, &s.FileSystems); err != nil {
		s.Problems["file systems"] = err.Error()
	}
	if err := purerest.List(ctx, c.Client, "/buckets", notDestroyed, &s.Buckets); err != nil {
		s.Problems["buckets"] = err.Error()
	}
	for _, p := range []string{"/file-system-replica-links", "/bucket-replica-links"} {
		if err := purerest.List(ctx, c.Client, p, nil, &s.Links); err != nil {
			s.Problems["replica links"] = err.Error()
		}
	}
	if err := purerest.List(ctx, c.Client, "/array-connections", nil, &s.Connections); err != nil {
		s.Problems["connections"] = err.Error()
	}
	if err := purerest.List(ctx, c.Client, "/alerts", url.Values{"filter": {"state='open'"}}, &s.Alerts); err != nil {
		s.Problems["alerts"] = err.Error()
	}
	// Performance is for history: not reading it isn't trouble with the array.
	var perf []purerest.Performance
	switch err := purerest.List(ctx, c.Client, "/arrays/performance", nil, &perf); {
	case err != nil:
		s.PerfErr = err.Error()
	case len(perf) == 0:
		s.PerfErr = "/arrays/performance listed nothing"
	default:
		s.Perf = &perf[0]
	}
	return s, nil
}
