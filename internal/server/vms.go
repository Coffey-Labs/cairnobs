package server

import (
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A VM's guest file systems, from its guest agent (docs/cluster-watcher.md,
// What pages): the fullest at these is a warning, then critical.
const (
	vmDiskWarn = 90
	vmDiskCrit = 95
)

// vmMigrationFor is how long a failed live migration stays a warning: the
// VM still runs where it was, but a drain or an update that needs it
// moved is stuck.
const vmMigrationFor = 24 * time.Hour

// factNum is a number fact as it comes off the wire (JSON's float64), or
// as a test builds it.
func factNum(f map[string]any, k string) (float64, bool) {
	switch v := f[k].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// vmExtras are a VM's conditions beside its state: a full guest disk and
// a failed live migration. A VM that isn't running has neither.
func vmExtras(src string, r report.Workload, now time.Time) []Condition {
	if r.State != report.StateOK && r.State != report.StateUpdating && r.State != report.StateDegraded {
		return nil
	}
	var out []Condition
	name, href := where(r.Name, r.Namespace), workloadHref(src, r.Namespace, r.Name)
	if pct, ok := factNum(r.Facts, "fullest_pct"); ok && pct >= vmDiskWarn {
		sev := "warning"
		if pct >= vmDiskCrit {
			sev = "critical"
		}
		mount, _ := r.Facts["fullest_mount"].(string)
		all, _ := r.Facts["filesystems"].(string)
		out = append(out, Condition{Key: "vmdisk/" + src + "/" + r.Namespace + "/" + r.Name, Severity: sev,
			Title:  fmt.Sprintf("VM %s (%s): %s is %.0f%% full", name, src, mount, pct),
			Detail: "From its guest agent: " + all + ".", Href: href})
	}
	if at, _ := r.Facts["migration_failed"].(string); at != "" {
		if t, err := time.Parse(time.RFC3339, at); err == nil && now.Sub(t) < vmMigrationFor {
			detail := "It still runs where it was; a drain or update that needs it moved will wait."
			if why, _ := r.Facts["migration_error"].(string); why != "" {
				detail = why + ". " + detail
			}
			out = append(out, Condition{Key: "vmmigration/" + src + "/" + r.Namespace + "/" + r.Name, Severity: "warning",
				Title:  fmt.Sprintf("VM %s (%s): live migration failed", name, src),
				Detail: detail, Href: href})
		}
	}
	return out
}
