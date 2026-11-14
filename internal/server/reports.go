package server

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

// ReportsConfig is the `reports` section of obs-beacond.yaml.
type ReportsConfig struct {
	Timezone string        `yaml:"timezone"` // months and days start here; default the server's own
	Digest   *DigestConfig `yaml:"digest"`   // the weekly digest; off when left out
	loc      *time.Location
}

func (c *ReportsConfig) check() error {
	if c.Timezone == "" {
		c.Timezone = serverZone()
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("reports: timezone: %w", err)
	}
	c.loc = loc
	if c.Digest != nil {
		return c.Digest.check(loc)
	}
	return nil
}

func (srv *Server) reportsLoc() *time.Location {
	if r := srv.cfg.Reports; r != nil && r.loc != nil {
		return r.loc
	}
	loc, err := time.LoadLocation(serverZone())
	if err != nil {
		return time.UTC
	}
	return loc
}

// serverZone names the server's own time zone, for reports that don't name
// one: TZ if set, else what /etc/localtime links to, else /etc/timezone,
// else UTC. A name rather than time.Local, so the browser can show reports
// in it too.
func serverZone() string {
	ok := func(name string) bool {
		_, err := time.LoadLocation(name)
		return name != "" && name != "Local" && err == nil
	}
	if tz := os.Getenv("TZ"); ok(tz) {
		return tz
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(p, "zoneinfo/"); i >= 0 && ok(p[i+len("zoneinfo/"):]) {
			return p[i+len("zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && ok(strings.TrimSpace(string(b))) {
		return strings.TrimSpace(string(b))
	}
	return "UTC"
}

// period turns "2026-10" (a month), "7d" or "30d" (ending now) into a range.
func period(p string, now time.Time, loc *time.Location) (from, to time.Time, label string, err error) {
	switch p {
	case "", "30d":
		return now.Add(-30 * 24 * time.Hour), now, "Last 30 days", nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), now, "Last 7 days", nil
	}
	m, err := time.ParseInLocation("2006-01", p, loc)
	if err != nil {
		return from, to, "", errors.New("period is YYYY-MM, 7d or 30d")
	}
	to = m.AddDate(0, 1, 0)
	if to.After(now) {
		to = now
	}
	return m, to, m.Format("January 2006"), nil
}

// Uptime is one entity over a period. Known is the time Cairn OBS was
// watching; Down is time critical, Degraded time warning or serious.
type Uptime struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	KnownS    float64 `json:"known_s"`
	DownS     float64 `json:"down_s"`
	DegradedS float64 `json:"degraded_s"`
	Pct       float64 `json:"pct"` // up share of known time; -1 when nothing is known
	Incidents int     `json:"incidents"`
}

// integrate adds up the time each entity spent in each status in [from, to).
func integrate(changes []state.Change, from, to time.Time) map[string]*Uptime {
	by := map[string][]state.Change{}
	for _, c := range changes {
		by[c.Entity] = append(by[c.Entity], c)
	}
	out := map[string]*Uptime{}
	for e, cs := range by {
		u := &Uptime{ID: e}
		for i, c := range cs {
			start := c.At
			if start.Before(from) {
				start = from
			}
			end := to
			if i+1 < len(cs) {
				end = cs[i+1].At
			}
			if !end.After(start) {
				continue
			}
			d := end.Sub(start).Seconds()
			switch c.Status {
			case "good", "off":
				u.KnownS += d
			case "warning", "serious":
				u.KnownS += d
				u.DegradedS += d
			case "critical":
				u.KnownS += d
				u.DownS += d
			}
		}
		u.Pct = -1
		if u.KnownS > 0 {
			u.Pct = 100 * (u.KnownS - u.DownS) / u.KnownS
		}
		out[e] = u
	}
	return out
}

// IncidentRow is one incident in a report.
type IncidentRow struct {
	ID       string      `json:"id"`
	Severity string      `json:"severity"`
	Title    string      `json:"title"`
	Host     string      `json:"host,omitempty"`
	Service  string      `json:"service,omitempty"`
	Opened   time.Time   `json:"opened"`
	Resolved *time.Time  `json:"resolved,omitempty"`
	AckBy    string      `json:"ack_by,omitempty"`
	ToAckS   *float64    `json:"to_ack_s,omitempty"` // from when people were told
	ToFixS   *float64    `json:"to_fix_s,omitempty"` // from when it started
	Note     *state.Note `json:"note,omitempty"`
}

// CertRow is one certificate and when it expires.
type CertRow struct {
	Name    string    `json:"name"`
	Where   string    `json:"where"`
	Source  string    `json:"source"` // check or cloudflare
	Expires time.Time `json:"expires"`
	Days    int       `json:"days"`
}

// DiskRow is one filesystem's fill and where it's heading.
type DiskRow struct {
	Host      string   `json:"host"`
	Mount     string   `json:"mount"`
	Used      uint64   `json:"used"`
	Total     uint64   `json:"total"`
	Pct       float64  `json:"pct"`
	PerDay    float64  `json:"per_day"`             // bytes a day, from the period's trend
	FullDays  *float64 `json:"full_days,omitempty"` // at that rate; nil when it isn't filling
	Projected string   `json:"projected,omitempty"` // the date, in the report's zone
}

// HostTrend is a host's daily CPU and memory over the period.
type HostTrend struct {
	Host   string    `json:"host"`
	Days   []string  `json:"days"`
	CPU    []float64 `json:"cpu"`
	CPUMax []float64 `json:"cpu_max"`
	Mem    []float64 `json:"mem"`
}

// MailReport is the mail monitor over the period.
type MailReport struct {
	Rounds   int      `json:"rounds"`
	Missed   int      `json:"missed"`
	MedianS  float64  `json:"median_s"`
	P95S     float64  `json:"p95_s"`
	MaxS     float64  `json:"max_s"`
	QueueMax int      `json:"queue_max"`
	Listed   []Uptime `json:"listed"` // block-list checks: time listed (degraded or down)
}

// BackupSummary is one host's backups now, and their trouble in the period.
type BackupSummary struct {
	Host      string   `json:"host"`
	Expected  int      `json:"expected"`
	Current   int      `json:"current"`
	Problems  []string `json:"problems"`
	Incidents int      `json:"incidents"`
}

// Report is everything on the Reports page for one period.
type Report struct {
	From         time.Time       `json:"from"`
	To           time.Time       `json:"to"`
	Label        string          `json:"label"`
	HistorySince time.Time       `json:"history_since,omitzero"`
	Services     []Uptime        `json:"services"`
	Hosts        []Uptime        `json:"hosts"`
	Checks       []Uptime        `json:"checks"`
	Incidents    []IncidentRow   `json:"incidents"`
	Certs        []CertRow       `json:"certs"`
	Disks        []DiskRow       `json:"disks"`
	Trends       []HostTrend     `json:"trends"`
	Mail         *MailReport     `json:"mail,omitempty"`
	Backups      []BackupSummary `json:"backups"`
}

func (srv *Server) buildReport(from, to, now time.Time, label string) (*Report, error) {
	if srv.db == nil {
		return nil, errors.New("reports need obs-beacond's database")
	}
	loc := srv.reportsLoc()
	r := &Report{From: from, To: to, Label: label, HistorySince: srv.db.HistoryStart(),
		Services: []Uptime{}, Hosts: []Uptime{}, Checks: []Uptime{}, Incidents: []IncidentRow{}, Certs: []CertRow{},
		Disks: []DiskRow{}, Trends: []HostTrend{}, Backups: []BackupSummary{}}

	// Uptime.
	changes, err := srv.db.StatusChanges(from, to)
	if err != nil {
		return nil, err
	}
	up := integrate(changes, from, to)
	topo := srv.currentTopology()
	names := map[string]string{}
	if topo != nil {
		for _, s := range topo.Services {
			names["svc/"+s.ID] = s.Name
		}
	}
	if c := srv.currentChecks(); c != nil {
		for _, s := range c.Checks {
			names["check/"+s.ID] = s.Name
		}
	}
	// Incidents, and which service each belongs to.
	incs, err := srv.db.IncidentsBetween(from, to)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, i := range incs {
		ids = append(ids, i.ID)
	}
	notes, err := srv.db.Notes(ids)
	if err != nil {
		return nil, err
	}
	perEntity := map[string]int{}
	for _, i := range incs {
		if i.Fired.IsZero() {
			continue // held back and never told: not an incident anyone saw
		}
		row := IncidentRow{ID: i.ID, Severity: i.Severity, Title: i.Title, Host: i.Host, Opened: i.Opened, AckBy: i.AckBy}
		if topo != nil {
			if n := nodeOf(topo, Condition{Key: i.Key, Host: i.Host}); n != "" {
				row.Service = n
				perEntity["svc/"+n]++
			}
		}
		if i.Host != "" {
			perEntity["host/"+i.Host]++
		}
		if id, ok := strings.CutPrefix(i.Key, "check/"); ok {
			perEntity["check/"+id]++
		}
		if !i.Resolved.IsZero() {
			t := i.Resolved
			row.Resolved = &t
			d := i.Resolved.Sub(i.Opened).Seconds()
			row.ToFixS = &d
		}
		if !i.AckAt.IsZero() {
			d := i.AckAt.Sub(i.Fired).Seconds()
			row.ToAckS = &d
		}
		if n, ok := notes[i.ID]; ok {
			row.Note = &n
		}
		r.Incidents = append(r.Incidents, row)
	}
	for e, u := range up {
		u.Name = names[e]
		u.Incidents = perEntity[e]
		kind, id, _ := strings.Cut(e, "/")
		if u.Name == "" {
			u.Name = id
		}
		u.ID = id
		switch kind {
		case "svc":
			r.Services = append(r.Services, *u)
		case "host":
			r.Hosts = append(r.Hosts, *u)
		case "check":
			r.Checks = append(r.Checks, *u)
		}
	}
	byPct := func(l []Uptime) {
		sort.Slice(l, func(i, j int) bool {
			if l[i].Pct != l[j].Pct {
				return l[i].Pct < l[j].Pct
			}
			return l[i].Name < l[j].Name
		})
	}
	byPct(r.Services)
	byPct(r.Hosts)
	byPct(r.Checks)

	// Certificates, as they stand now.
	checks, _ := srv.judgeChecks(now)
	for _, c := range checks {
		var soonest time.Time
		for _, res := range c.Results {
			if !res.CertExpires.IsZero() && (soonest.IsZero() || res.CertExpires.Before(soonest)) {
				soonest = res.CertExpires
			}
		}
		if !soonest.IsZero() {
			r.Certs = append(r.Certs, CertRow{Name: c.Name, Where: c.Target, Source: "check", Expires: soonest, Days: int(soonest.Sub(now).Hours() / 24)})
		}
	}
	if srv.cf != nil {
		for _, z := range srv.cf.Snapshot().Zones {
			for _, c := range z.Certs {
				if !c.Expires.IsZero() {
					r.Certs = append(r.Certs, CertRow{Name: z.Name + " edge", Where: strings.Join(c.Hosts, ", "), Source: "cloudflare", Expires: c.Expires, Days: int(c.Expires.Sub(now).Hours() / 24)})
				}
			}
		}
	}
	if srv.ak != nil {
		for _, e := range srv.ak.Snapshot().Enrollments {
			var soonest time.Time
			for _, c := range e.Deployed {
				if soonest.IsZero() || c.NotAfter.Before(soonest) {
					soonest = c.NotAfter
				}
			}
			if !soonest.IsZero() {
				r.Certs = append(r.Certs, CertRow{Name: e.CN, Where: akamaiEnrollment(e), Source: "akamai", Expires: soonest, Days: int(soonest.Sub(now).Hours() / 24)})
			}
		}
	}
	sort.Slice(r.Certs, func(i, j int) bool { return r.Certs[i].Expires.Before(r.Certs[j].Expires) })

	// Capacity: the period's hourly samples.
	hours, err := srv.db.HostHours(from, to)
	if err != nil {
		return nil, err
	}
	r.Disks, r.Trends = capacity(hours, loc)
	r.Disks, r.Trends = nonNilSlice(r.Disks), nonNilSlice(r.Trends) // [] rather than null before the first hour

	// Mail.
	if srv.mail != nil {
		rounds, err := srv.db.MailRounds(from, to)
		if err != nil {
			return nil, err
		}
		m := mailReport(rounds)
		for _, c := range checks {
			if c.Kind == "rbl" {
				if u, ok := up["check/"+c.ID]; ok {
					x := *u
					x.ID, x.Name = c.ID, c.Name
					m.Listed = append(m.Listed, x)
				}
			}
		}
		r.Mail = m
	}

	// Backups, as they stand now, and their incidents in the period.
	if f := srv.currentBackups(); f != nil {
		conds := srv.backupConditions(now)
		scans := srv.backupScans(now)
		for host, spec := range f.Hosts {
			s := BackupSummary{Host: host, Problems: []string{}}
			for _, src := range spec.Received {
				s.Expected += len(src.Expect)
			}
			s.Expected += len(spec.Made)
			bad := map[string]bool{}
			for _, c := range conds {
				if c.Host == host {
					s.Problems = append(s.Problems, c.Title)
					if strings.Contains(c.Key, "/received/") || strings.Contains(c.Key, "/made/") {
						bad[c.Key] = true
					}
				}
			}
			if _, ok := scans[host]; ok {
				s.Current = s.Expected - len(bad)
			}
			for _, i := range incs {
				if strings.HasPrefix(i.Key, "backup/"+host+"/") && !i.Fired.IsZero() {
					s.Incidents++
				}
			}
			r.Backups = append(r.Backups, s)
		}
		sort.Slice(r.Backups, func(i, j int) bool { return r.Backups[i].Host < r.Backups[j].Host })
	}
	return r, nil
}

// capacity fits a straight line through each disk's use to say when it
// fills, and averages CPU and memory per day.
func capacity(hours []state.HostHour, loc *time.Location) ([]DiskRow, []HostTrend) {
	type pt struct{ x, y float64 }
	series := map[[2]string][]pt{}
	last := map[[2]string]state.DiskSample{}
	lastAt := map[[2]string]time.Time{}
	type day struct {
		n                int
		cpu, cpuMax, mem float64
	}
	days := map[string]map[string]*day{}
	var order []string
	for _, h := range hours {
		for _, d := range h.Disks {
			k := [2]string{h.Host, d.Mount}
			series[k] = append(series[k], pt{float64(h.Hour.Unix()), float64(d.Used)})
			last[k], lastAt[k] = d, h.Hour
		}
		key := h.Hour.In(loc).Format("2006-01-02")
		if days[h.Host] == nil {
			days[h.Host] = map[string]*day{}
		}
		dd := days[h.Host][key]
		if dd == nil {
			dd = &day{}
			days[h.Host][key] = dd
			if !contains(order, key) {
				order = append(order, key)
			}
		}
		dd.n++
		dd.cpu += h.CPU
		dd.mem += h.Mem
		dd.cpuMax = math.Max(dd.cpuMax, h.CPUMax)
	}
	var disks []DiskRow
	for k, pts := range series {
		d := last[k]
		row := DiskRow{Host: k[0], Mount: k[1], Used: d.Used, Total: d.Total}
		if d.Total > 0 {
			row.Pct = 100 * float64(d.Used) / float64(d.Total)
		}
		if len(pts) >= 24 { // a day of samples before a trend means anything
			var sx, sy, sxx, sxy float64
			for _, p := range pts {
				sx += p.x
				sy += p.y
				sxx += p.x * p.x
				sxy += p.x * p.y
			}
			n := float64(len(pts))
			if den := n*sxx - sx*sx; den != 0 {
				slope := (n*sxy - sx*sy) / den // bytes per second
				row.PerDay = slope * 86400
				if slope > 0 && d.Total > d.Used {
					days := float64(d.Total-d.Used) / row.PerDay
					row.FullDays = &days
					if days < 3650 {
						row.Projected = lastAt[k].Add(time.Duration(days * 24 * float64(time.Hour))).In(loc).Format("2006-01-02")
					}
				}
			}
		}
		disks = append(disks, row)
	}
	sort.Slice(disks, func(i, j int) bool {
		if disks[i].Host != disks[j].Host {
			return disks[i].Host < disks[j].Host
		}
		return disks[i].Mount < disks[j].Mount
	})
	sort.Strings(order)
	var trends []HostTrend
	for host, m := range days {
		t := HostTrend{Host: host}
		for _, k := range order {
			if d := m[k]; d != nil {
				t.Days = append(t.Days, k)
				t.CPU = append(t.CPU, d.cpu/float64(d.n))
				t.CPUMax = append(t.CPUMax, d.cpuMax)
				t.Mem = append(t.Mem, d.mem/float64(d.n))
			}
		}
		trends = append(trends, t)
	}
	sort.Slice(trends, func(i, j int) bool { return trends[i].Host < trends[j].Host })
	return disks, trends
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func mailReport(rounds []state.MailRound) *MailReport {
	m := &MailReport{Rounds: len(rounds), Listed: []Uptime{}}
	var secs []float64
	for _, r := range rounds {
		if r.Seconds < 0 {
			m.Missed++
		} else {
			secs = append(secs, r.Seconds)
		}
		m.QueueMax = max(m.QueueMax, r.Queue)
	}
	if len(secs) > 0 {
		sort.Float64s(secs)
		m.MedianS = secs[len(secs)/2]
		m.P95S = secs[min(len(secs)-1, int(math.Ceil(0.95*float64(len(secs))))-1)]
		m.MaxS = secs[len(secs)-1]
	}
	return m
}
