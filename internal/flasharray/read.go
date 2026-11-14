package flasharray

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/cairnobs/cairnobs/internal/purerest"
)

type ref struct {
	Name string `json:"name"`
}

// Space is how an array, a pod or a volume uses its flash.
type Space struct {
	DataReduction  float64 `json:"data_reduction"`
	TotalReduction float64 `json:"total_reduction"`
	TotalPhysical  int64   `json:"total_physical"`
	Unique         int64   `json:"unique"`
	Snapshots      int64   `json:"snapshots"`
	System         int64   `json:"system"`
	Virtual        int64   `json:"virtual"`
}

// Array is /arrays' one item.
type Array struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	OS       string   `json:"os"` // Purity//FA
	Version  string   `json:"version"`
	Capacity int64    `json:"capacity"`
	Parity   *float64 `json:"parity"` // below 1.0 while redundancy is rebuilt
	Space    Space    `json:"space"`
}

// Controller is one of /controllers.
type Controller struct {
	Name    string `json:"name"`   // CT0, CT1, SH9.SC0
	Mode    string `json:"mode"`   // primary, secondary, offline, not present
	Status  string `json:"status"` // ready, not ready, updating, unknown
	Type    string `json:"type"`   // array_controller, shelf_controller
	Model   string `json:"model"`
	Version string `json:"version"`
}

// Part is one of /hardware or /drives: only what isn't healthy is kept.
type Part struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details"`
	Type    string `json:"type"`
}

// Volume is one of /volumes, not destroyed.
type Volume struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Serial          string `json:"serial"`
	Provisioned     int64  `json:"provisioned"`
	ConnectionCount int    `json:"connection_count"`
	Created         int64  `json:"created"`
	Pod             *ref   `json:"pod"`
	VolumeGroup     *ref   `json:"volume_group"`
	PromotionStatus string `json:"promotion_status"`
	Subtype         string `json:"subtype"` // regular, protocol_endpoint
	Space           Space  `json:"space"`
}

// PodArray is one array's status in a pod.
type PodArray struct {
	Name           string  `json:"name"`
	Status         string  `json:"status"` // online, offline, resyncing, suspended, unknown
	MediatorStatus string  `json:"mediator_status"`
	Progress       float64 `json:"progress"`
	FrozenAt       *int64  `json:"frozen_at"`
}

// Pod is one of /pods: ActiveCluster, or a pod for continuous replication.
type Pod struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Destroyed       bool       `json:"destroyed"`
	Arrays          []PodArray `json:"arrays"`
	PromotionStatus string     `json:"promotion_status"`
}

// Connection is one of /array-connections: replication to another array.
type Connection struct {
	ID      string `json:"id"`
	Name    string `json:"name"`   // the other array
	Status  string `json:"status"` // connected, connecting, partially_connected, unbalanced
	Type    string `json:"type"`   // async-replication, sync-replication
	Version string `json:"version"`
}

// Alert is one of /alerts, open.
type Alert = purerest.Alert

// Performance is /arrays/performance's one item.
type Performance = purerest.Performance

// Snapshot is one read of an array.
type Snapshot struct {
	Name        string // what it's kept under
	Array       Array
	Controllers []Controller
	Unhealthy   []Part // hardware and drives that aren't fine
	Volumes     []Volume
	Pods        []Pod
	Connections []Connection
	Alerts      []Alert
	Perf        *Performance // nil when it couldn't be read: PerfErr says why
	PerfErr     string
	Problems    map[string]string
	At          time.Time
}

// healthy are the hardware and drive states that aren't trouble.
var healthy = map[string]bool{"healthy": true, "unused": true, "identifying": true, "not_installed": true, "empty": true, "": true}

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
		return nil, fmt.Errorf("flasharray: /arrays listed nothing")
	}
	s.Array = arrays[0]
	if err := purerest.List(ctx, c.Client, "/controllers", nil, &s.Controllers); err != nil {
		s.Problems["controllers"] = err.Error()
	}
	for _, p := range []string{"/hardware", "/drives"} {
		var parts []Part
		if err := purerest.List(ctx, c.Client, p, nil, &parts); err != nil {
			s.Problems[p[1:]] = err.Error()
		}
		for _, x := range parts {
			if !healthy[x.Status] {
				s.Unhealthy = append(s.Unhealthy, x)
			}
		}
	}
	if err := purerest.List(ctx, c.Client, "/volumes", url.Values{"destroyed": {"false"}}, &s.Volumes); err != nil {
		s.Problems["volumes"] = err.Error()
	}
	if err := purerest.List(ctx, c.Client, "/pods", url.Values{"destroyed": {"false"}}, &s.Pods); err != nil {
		s.Problems["pods"] = err.Error()
	}
	if err := purerest.List(ctx, c.Client, "/array-connections", nil, &s.Connections); err != nil {
		s.Problems["connections"] = err.Error()
	}
	if err := purerest.List(ctx, c.Client, "/alerts", url.Values{"filter": {"state='open'"}}, &s.Alerts); err != nil {
		s.Problems["alerts"] = err.Error()
	}
	// Performance is for history: not reading it isn't trouble with the array.
	var perf []Performance
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
