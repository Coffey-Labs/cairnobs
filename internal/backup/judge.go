package backup

import (
	"fmt"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Finding is one thing wrong with a host's backups; obs-beacond makes it a
// condition. Missing or stale backups are serious (email and Discord, not
// the phone); a failing disk or degraded RAID is critical.
type Finding struct {
	Key      string // unique per host, e.g. received/node2/wiki-db
	Severity string
	Title    string
	Detail   string
}

// Judge compares host's latest scan with what backups.yaml expects.
func (f *File) Judge(host string, b *report.Backups, now time.Time, scanEvery time.Duration) []Finding {
	h, ok := f.Hosts[host]
	if !ok {
		return nil
	}
	var out []Finding
	add := func(key, sev, title, detail string) {
		out = append(out, Finding{Key: key, Severity: sev, Title: title, Detail: detail})
	}
	if b == nil {
		add("scan", "warning", "No backup scan from "+host, "The obs-beacon-scan timer hasn't written a scan obs-beacon-agent could read. systemctl status obs-beacon-scan.timer")
		return out
	}
	if age := now.Sub(b.At); age > 4*scanEvery {
		add("scan", "warning", "The backup scan on "+host+" is "+ago(age)+" old",
			"The obs-beacon-scan timer has stopped. systemctl status obs-beacon-scan.timer")
	}
	for _, e := range b.Errors {
		add("scan-error/"+e, "warning", "The backup scan on "+host+" hit a problem", e+".")
	}
	byName := func(list []report.BackupDir, from, name string) *report.BackupDir {
		for i := range list {
			if list[i].Name == name && list[i].From == from {
				return &list[i]
			}
		}
		return nil
	}
	judge := func(key, what string, d *report.BackupDir, maxAge time.Duration) {
		switch {
		case d == nil || d.Count == 0:
			where := ""
			if d != nil {
				where = " in " + d.Path
			}
			add(key, "serious", what+": nothing on "+host, "No copy has arrived"+where+".")
		case d.Size == 0:
			add(key, "serious", what+" on "+host+" is empty", d.Path+" is 0 bytes.")
		case now.Sub(d.Modified) > maxAge:
			add(key, "serious", fmt.Sprintf("%s on %s is %s old", what, host, ago(now.Sub(d.Modified))),
				fmt.Sprintf("The newest copy is %s (%s); they should be under %s.", d.Path, Human(d.Size), ago(maxAge)))
		case d.PrevSize > 0 && float64(d.Size) < f.Shrink*float64(d.PrevSize):
			add(key, "warning", fmt.Sprintf("%s on %s shrank to %s", what, host, Human(d.Size)),
				fmt.Sprintf("The copy before was %s. Backups grow slowly; this usually means something was left out.", Human(d.PrevSize)))
		}
	}
	for _, s := range h.Received {
		for _, name := range s.Expect {
			judge("received/"+s.From+"/"+name, s.From+"'s "+name+" backup", byName(b.Received, s.From, name), s.MaxAge)
		}
	}
	for _, m := range h.Made {
		judge("made/"+m.Name, "The "+m.Name+" backup", byName(b.Made, "", m.Name), m.MaxAge)
	}
	for _, r := range b.Roots {
		if r.UsedPct >= 100*f.DiskWarn {
			add("disk/"+r.Path, "warning", fmt.Sprintf("Backup disk on %s is %.0f%% full", host, r.UsedPct),
				r.Path+" is where the other hosts' backups land. Old copies or a bigger disk.")
		}
	}
	for _, unit := range h.Jobs {
		var j *report.BackupJob
		for i := range b.Jobs {
			if b.Jobs[i].Unit == unit {
				j = &b.Jobs[i]
			}
		}
		switch {
		case j == nil:
		case j.Result == "":
			add("job/"+unit, "warning", unit+" is missing on "+host, "backups.yaml lists it, but systemd doesn't know it.")
		case j.Result != "success":
			add("job/"+unit, "serious", unit+" failed on "+host,
				fmt.Sprintf("Its last run ended with %s. journalctl -u %s", strings.ReplaceAll(j.Result, "-", " "), unit))
		}
	}
	for _, r := range b.RAID {
		if strings.Contains(r.State, "_") {
			add("raid/"+r.Dev, "critical", fmt.Sprintf("RAID %s on %s is degraded %s", r.Dev, host, r.State),
				"A disk has dropped out of the mirror; the other one is all there is. cat /proc/mdstat")
		}
	}
	for _, d := range b.SMART {
		switch {
		case d.Error != "":
			add("smart/"+d.Disk, "warning", "Can't read /dev/"+d.Disk+"'s health on "+host, d.Error+".")
		case !d.OK:
			add("smart/"+d.Disk, "critical", "/dev/"+d.Disk+" on "+host+" is failing", "SMART's overall health check did not pass. smartctl -a /dev/"+d.Disk)
		}
	}
	return out
}

func ago(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}

// Human is a size in KB, MB, GB.
func Human(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f TB", f)
}
