package hmc

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Server is one managed system: a Power server, as the HMC's
// ManagedSystem says it.
type Server struct {
	UUID          string      `xml:"Metadata>Atom>AtomID"`
	Name          string      `xml:"SystemName"`
	State         string      `xml:"State"` // operating, standby, power off, error, no connection, …
	DetailedState string      `xml:"DetailedState"`
	MachineType   string      `xml:"MachineTypeModelAndSerialNumber>MachineType"`
	Model         string      `xml:"MachineTypeModelAndSerialNumber>Model"`
	Serial        string      `xml:"MachineTypeModelAndSerialNumber>SerialNumber"`
	ProcUnits     float64     `xml:"AssociatedSystemProcessorConfiguration>InstalledSystemProcessorUnits"`
	ProcFree      float64     `xml:"AssociatedSystemProcessorConfiguration>CurrentAvailableSystemProcessorUnits"`
	MemMB         int64       `xml:"AssociatedSystemMemoryConfiguration>InstalledSystemMemory"`
	MemFreeMB     int64       `xml:"AssociatedSystemMemoryConfiguration>CurrentAvailableSystemMemory"`
	AttentionLED  string      `xml:"PhysicalSystemAttentionLEDState"` // "true" when a serviceable event is open
	Partitions    []Partition `xml:"-"`
}

// Partition is one logical partition, or a Virtual I/O Server: the same
// fields in both feeds.
type Partition struct {
	UUID       string  `xml:"PartitionUUID"`
	ID         int     `xml:"PartitionID"`
	Name       string  `xml:"PartitionName"`
	State      string  `xml:"PartitionState"` // running, not activated, open firmware, error, …
	Type       string  `xml:"PartitionType"`  // OS400, AIX/Linux, Virtual IO Server
	OS         string  `xml:"OperatingSystemVersion"`
	RefCode    string  `xml:"ReferenceCode"`
	RMC        string  `xml:"ResourceMonitoringControlState"` // active, inactive, none, unknown, busy
	Attention  string  `xml:"IsVirtualServiceAttentionLEDOn"`
	MemMB      int64   `xml:"PartitionMemoryConfiguration>CurrentMemory"`
	Dedicated  string  `xml:"PartitionProcessorConfiguration>CurrentHasDedicatedProcessors"`
	DedProcs   int     `xml:"PartitionProcessorConfiguration>CurrentDedicatedProcessorConfiguration>CurrentProcessors"`
	ProcUnits  float64 `xml:"PartitionProcessorConfiguration>CurrentSharedProcessorConfiguration>CurrentProcessingUnits"`
	VirtProcs  int     `xml:"PartitionProcessorConfiguration>CurrentSharedProcessorConfiguration>AllocatedVirtualProcessors"`
	SharedMode string  `xml:"PartitionProcessorConfiguration>CurrentSharingMode"`
}

// VIOS says whether the partition is a Virtual I/O Server.
func (p Partition) VIOS() bool { return p.Type == "Virtual IO Server" }

// Snapshot is one read of the HMC.
type Snapshot struct {
	Name     string // the HMC's host name
	Console  string // what the HMC calls itself
	Servers  []Server
	Problems map[string]string // what couldn't be read, by what it was
	At       time.Time
}

// feed is an Atom feed of one kind of object: each entry's content holds
// one, under a namespace Go's decoder doesn't need to know.
type feed[T any] struct {
	Entries []struct {
		Content struct {
			Item T `xml:",any"`
		} `xml:"content"`
	} `xml:"entry"`
}

func items[T any](f *feed[T]) []T {
	out := make([]T, 0, len(f.Entries))
	for _, e := range f.Entries {
		out = append(out, e.Content.Item)
	}
	return out
}

// Read reads the console, every managed system, and each one's partitions
// and Virtual I/O Servers: all GETs, with group=None, which leaves out the
// large optional parts (storage and network mappings).
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Name: c.name, Problems: map[string]string{}, At: now}
	var mc feed[struct {
		Name string `xml:"ManagementConsoleName"`
	}]
	if _, err := c.get(ctx, "/uom/ManagementConsole?group=None", &mc); err != nil {
		return nil, err
	}
	if cs := items(&mc); len(cs) > 0 {
		s.Console = strings.TrimSpace(cs[0].Name)
	}
	var ms feed[Server]
	if _, err := c.get(ctx, "/uom/ManagedSystem?group=None", &ms); err != nil {
		return nil, fmt.Errorf("hmc: managed systems: %w", err)
	}
	s.Servers = items(&ms)
	for i := range s.Servers {
		sv := &s.Servers[i]
		sv.UUID = strings.TrimSpace(sv.UUID)
		if sv.UUID == "" {
			continue
		}
		for _, kind := range []string{"LogicalPartition", "VirtualIOServer"} {
			var pf feed[Partition]
			if _, err := c.get(ctx, "/uom/ManagedSystem/"+sv.UUID+"/"+kind+"?group=None", &pf); err != nil {
				// A server the HMC can't reach can't list them either: its own
				// state says so. Only an operating one's failure is news.
				if strings.EqualFold(sv.State, "operating") {
					s.Problems[sv.Name+" "+kind] = err.Error()
				}
				continue
			}
			sv.Partitions = append(sv.Partitions, items(&pf)...)
		}
	}
	return s, nil
}
