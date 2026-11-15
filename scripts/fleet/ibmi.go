package main

import (
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/ibmi"
	"github.com/cairnobs/cairnobs/internal/report"
)

// IBM i partitions, as a host agent with an ibmi: section sends them
// (docs/ibmi.md): fleet-0041's agent reads them, each a source of its own.
// The records are the reader's own, made from snapshots as the views
// would answer.
const ibmiHost = 41

// ibmiPartitions are the partitions, in the order -ibmi N takes them.
var ibmiPartitions = []string{"prod1", "prod2", "ha1", "dev1"}

// newIBMi is the k'th partition: prod1 busy, its system ASP past IBM i's
// threshold; ha1 the PowerHA standby, its independent ASP varied off;
// dev1 on 7.4 without the authority to read its Technology Refresh.
func newIBMi(name, token string, k int) *fakeCluster {
	part := ibmiPartitions[k]
	c := &fakeCluster{name: name, token: token, recs: map[string]report.Workload{}, changed: map[string]bool{}, noUsage: true, source: part}
	units := []float64{4, 3, 2, 1}[k]
	c.usageFn = func(now time.Time) []report.Usage {
		day := 0.5 + 0.5*math.Sin(2*math.Pi*(float64(now.Hour()*60+now.Minute())/1440-0.3))
		base := []float64{68, 52, 6, 12}[k]
		if k == 0 && now.Hour() >= 1 && now.Hour() < 4 {
			base = 95 // the nightly batch
		}
		pct := min(100, base*(0.6+0.4*day)*(0.9+0.2*rand.Float64()))
		return []report.Usage{{ID: report.ClusterSelf, CPU: math.Round(pct*units) / 100, CPUOf: units}}
	}
	sysUsed := []float64{91.4, 72.8, 64.1, 48.0}[k]
	s := &ibmi.Snapshot{Name: part, Release: "7.5", TR: 6,
		System: ibmi.System{HostName: "S" + part, PartitionName: strings.ToUpper(part), PartitionID: k + 2, Partitions: 6, MachineType: "9105", MachineModel: "42A",
			Serial: "78ABC12", CPUs: int(units * 2), Capacity: units, MainStorageKB: int64(units*64) << 20, SystemASPMB: 2_400_000,
			SystemASPUsed: sysUsed, TempMB: 38_000, Jobs: []int{18204, 14550, 6120, 3307}[k], MaxJobs: 163520,
			ActiveJobs: []int{612, 488, 201, 97}[k], Threads: []int{2870, 2210, 890, 402}[k]},
		ASPs: []ibmi.ASP{{Number: 1, State: "NONE", Type: "SYSTEM", Units: 8, Present: "ALL", TotalMB: 2_400_000,
			FreeMB: int64(2_400_000 * (100 - sysUsed) / 100), Threshold: 90}}}
	switch part {
	case "prod1", "ha1":
		ia := ibmi.ASP{Number: 33, Name: "PRODIASP", State: "AVAILABLE", Type: "PRIMARY", Units: 6, Present: "ALL", TotalMB: 3_600_000, FreeMB: 1_440_000, Threshold: 90}
		if part == "ha1" {
			ia.State = "VARIED OFF"
		}
		s.ASPs = append(s.ASPs, ia)
	case "dev1":
		s.Release, s.TR = "7.4", 0
	}
	now := time.Now()
	for _, r := range ibmi.Records(s) {
		r.Since = now.Add(-time.Duration(40+k*7) * 24 * time.Hour)
		c.recs[r.ID] = r
	}
	return c
}
