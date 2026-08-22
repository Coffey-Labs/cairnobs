package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"strconv"
	"time"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// Metrics records carry the exact attribute contract the Hosts page
// reads (see web/src/lib/api.ts's getHostMetrics/listMetricsHosts): a
// `cairnobs.metrics=true` tag plus utilization and static-context
// fields, shipped as an ordinary tagged LogRecord because the query
// language maps any non-standard field name to attributes['field'] with
// automatic numeric casting -- no separate metrics pipeline exists, by
// design. Heartbeat records use the same trick with
// `cairnobs.heartbeat=true`, which is what the absence-style alert rules
// watch for.

// phaseOf gives each host its own deterministic offset into the wander
// functions below, so two hosts with the same baseline don't move in
// lockstep across the fleet's charts.
func phaseOf(name string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return float64(h.Sum32()%1000) / 1000 * 2 * math.Pi
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// metricsRecord samples host h at time t. backfillStart anchors the
// disk-growth trend so a host that's "filling up" is at its lowest at
// the oldest end of the window and its highest right now, in whichever
// order the samples happen to be generated.
func metricsRecord(h *host, t, backfillStart time.Time, r *rand.Rand) *logsv1.LogRecord {
	phase := phaseOf(h.name)
	mins := float64(t.Unix()) / 60

	// Two sine components of different periods plus noise: a slow
	// business-hours swell and a faster one, so a CPU chart looks like a
	// machine doing work rather than a random walk.
	cpu := h.cpuBase * (1 +
		0.45*math.Sin(mins/97+phase) +
		0.2*math.Sin(mins/13+phase*2))
	cpu = clamp(cpu*diurnal(t)+r.NormFloat64()*2.5, 0.4, 99)

	memFrac := clamp(h.memFrac*(1+0.08*math.Sin(mins/211+phase))+r.NormFloat64()*0.01, 0.03, 0.97)

	days := t.Sub(backfillStart).Hours() / 24
	diskFrac := clamp(h.diskFrac+h.diskGrowthPerDay*days+r.NormFloat64()*0.002, 0.02, 0.985)

	// A fixed boot moment per host, far enough back that uptimes read
	// like real long-lived servers (and differ from each other).
	bootOffset := time.Duration(3+int(phase*17)) * 24 * time.Hour
	uptime := int64(t.Add(bootOffset).Sub(backfillStart).Seconds())

	return newRecord(h, h.service, t, logsv1.Severity_SEVERITY_INFO, "host metrics", map[string]string{
		"cairnobs.metrics": "true",
		"cpu_percent":      fmt.Sprintf("%.2f", cpu),
		"mem_used_bytes":   strconv.FormatInt(int64(float64(h.memTotal)*memFrac), 10),
		"mem_total_bytes":  strconv.FormatInt(h.memTotal, 10),
		"disk_used_bytes":  strconv.FormatInt(int64(float64(h.diskTot)*diskFrac), 10),
		"disk_total_bytes": strconv.FormatInt(h.diskTot, 10),
		"cpu_cores":        strconv.Itoa(h.cores),
		"os_name":          h.os,
		"kernel_version":   h.kernel,
		"arch":             h.arch,
		"uptime_seconds":   strconv.FormatInt(uptime, 10),
		"ipv4_addresses":   h.ipv4,
		"ipv6_addresses":   h.ipv6,
	})
}

func heartbeatRecord(h *host, t time.Time) *logsv1.LogRecord {
	return newRecord(h, h.service, t, logsv1.Severity_SEVERITY_INFO, "agent heartbeat", map[string]string{
		"cairnobs.heartbeat": "true",
		"agent_version":      h.agentVersion,
	})
}
