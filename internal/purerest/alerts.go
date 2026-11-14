package purerest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Alert is one of /alerts, open.
type Alert struct {
	ID            string `json:"id"`
	Name          string `json:"name"` // its number
	Code          int    `json:"code"`
	Severity      string `json:"severity"` // info, warning, critical, hidden
	State         string `json:"state"`
	Summary       string `json:"summary"`
	ComponentName string `json:"component_name"`
	ComponentType string `json:"component_type"`
	Created       int64  `json:"created"`
}

// Performance is /arrays/performance's one item, on either kind of array:
// what it did over its last few seconds, for every protocol together.
type Performance struct {
	ReadsPerSec      float64 `json:"reads_per_sec"`
	WritesPerSec     float64 `json:"writes_per_sec"`
	UsecPerReadOp    float64 `json:"usec_per_read_op"`
	UsecPerWriteOp   float64 `json:"usec_per_write_op"`
	ReadBytesPerSec  float64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec float64 `json:"write_bytes_per_sec"`
}

// NoteAlerts lists Purity's open alerts on a record. A critical one makes
// a record that's otherwise fine degraded: Purity judged it, and says why.
func NoteAlerts(r *report.Workload, as []Alert) {
	if len(as) == 0 {
		return
	}
	var all, crit []string
	for _, a := range as {
		t := a.Summary
		if t == "" {
			t = fmt.Sprintf("alert %d", a.Code)
		}
		if a.ComponentName != "" && !strings.EqualFold(a.ComponentName, r.Name) {
			t = a.ComponentName + ": " + t
		}
		all = append(all, fmt.Sprintf("%s (%s)", t, a.Severity))
		if a.Severity == "critical" {
			crit = append(crit, t)
		}
	}
	sort.Strings(all)
	if r.Facts == nil {
		r.Facts = map[string]any{}
	}
	r.Facts["alerts"] = strings.Join(all, ", ")
	if len(crit) > 0 && r.State == report.StateOK {
		sort.Strings(crit)
		r.State, r.Word = report.StateDegraded, "Alert from Purity: "+strings.Join(crit, ", ")
	}
}
