package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state"
)

// DigestConfig is reports.digest in obs-beacond.yaml: a weekly summary, so a
// quiet week reads as "all clear" rather than as silence.
type DigestConfig struct {
	Weekday  string   `yaml:"weekday"`  // default monday
	At       string   `yaml:"at"`       // HH:MM in the reports' timezone; default 08:00
	Channels []string `yaml:"channels"` // alert channel ids; default every email channel
	day      time.Weekday
	minute   int // minutes after midnight
}

func (d *DigestConfig) check(*time.Location) error {
	if d.Weekday == "" {
		d.Weekday = "monday"
	}
	found := false
	for i := time.Sunday; i <= time.Saturday; i++ {
		if strings.EqualFold(i.String(), d.Weekday) {
			d.day, found = i, true
		}
	}
	if !found {
		return fmt.Errorf("reports: digest: weekday %q", d.Weekday)
	}
	if d.At == "" {
		d.At = "08:00"
	}
	t, err := time.Parse("15:04", d.At)
	if err != nil {
		return fmt.Errorf("reports: digest: at %q is not HH:MM", d.At)
	}
	d.minute = t.Hour()*60 + t.Minute()
	return nil
}

// digestChannels are the channels the digest goes to.
func (srv *Server) digestChannels() []notify.Channel {
	d := srv.cfg.Reports.Digest
	var out []notify.Channel
	for _, ch := range srv.alerts.chans() {
		if len(d.Channels) == 0 && ch.Type() == "email" || contains(d.Channels, ch.ID()) {
			out = append(out, ch)
		}
	}
	return out
}

// runDigest sends the digest once a week, at the set time, and remembers
// that it did so a restart doesn't send it twice.
func (srv *Server) runDigest(ctx context.Context) {
	if srv.cfg.Reports == nil || srv.cfg.Reports.Digest == nil || srv.db == nil {
		return
	}
	d, loc := srv.cfg.Reports.Digest, srv.reportsLoc()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			local := now.In(loc)
			today := local.Format("2006-01-02")
			if local.Weekday() != d.day || local.Hour()*60+local.Minute() < d.minute || srv.db.Meta("digest_last") == today {
				continue
			}
			_ = srv.db.SetMeta("digest_last", today) // first, so a failure isn't retried every minute
			if err := srv.sendDigest(ctx, now); err != nil {
				log.Printf("digest: %v", err)
			}
		}
	}
}

func (srv *Server) sendDigest(ctx context.Context, now time.Time) error {
	subject, text, short, err := srv.digest(now)
	if err != nil {
		return err
	}
	url := srv.cfg.Alerts.PublicURL + "/reports?period=7d"
	var errs []string
	for _, ch := range srv.digestChannels() {
		n := notify.Notice{Kind: "digest", Severity: "info", Title: subject, Detail: text, URL: url, Since: now}
		if ch.Type() != "email" {
			n.Detail = short
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := ch.Send(sctx, n); err != nil {
			errs = append(errs, err.Error())
		} else {
			log.Printf("digest: sent to %s", ch.ID())
		}
		cancel()
	}
	srv.audit("obs-beacond", "sent the weekly digest", subject)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// digest writes the week's summary: the full text for email and a few
// lines for chat.
func (srv *Server) digest(now time.Time) (subject, text, short string, err error) {
	loc := srv.reportsLoc()
	from := now.Add(-7 * 24 * time.Hour)
	r, err := srv.buildReport(from, now, now, "Last 7 days")
	if err != nil {
		return "", "", "", err
	}
	_, conds := srv.view(now)
	conds = srv.groupConditions(conds, srv.alerts.firing)
	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	span := from.In(loc).Format("Jan 2") + " to " + now.In(loc).Format("Jan 2, 2006")

	var worry []string
	for _, c := range conds {
		if c.Severity != "warning" {
			worry = append(worry, c.Title)
		}
	}
	verdict := "all clear"
	if len(worry) > 0 {
		verdict = plural(len(worry), "thing needs", "things need") + " a look"
	}
	subject = fmt.Sprintf("Weekly digest: %s (%s)", verdict, span)
	line("Cairn OBS: the week of %s (times in %s).", span, now.In(loc).Format("MST"))
	line("")

	if len(conds) == 0 {
		line("Right now everything is fine.")
	} else {
		line("Right now:")
		for _, c := range conds {
			line("  - %s: %s", strings.ToUpper(c.Severity[:1])+c.Severity[1:], c.Title)
		}
	}
	line("")

	// Uptime: services below 100% by name, the rest counted.
	line("Uptime")
	perfect := 0
	for _, u := range r.Services {
		if u.Pct < 0 {
			continue
		}
		if u.DownS == 0 && u.Incidents == 0 {
			perfect++
			continue
		}
		down := "no downtime"
		if u.DownS > 0 {
			down = dur(u.DownS) + " down"
		}
		line("  %-28s %7s  %-14s %s", u.Name, pctText(u.Pct), down, plural(u.Incidents, "incident", "incidents"))
	}
	if perfect > 0 {
		line("  %s up the whole week.", plural(perfect, "other service", "other services"))
	}
	if !r.HistorySince.IsZero() && r.HistorySince.After(from) {
		line("  (Cairn OBS has kept history since %s.)", r.HistorySince.In(loc).Format("Jan 2 15:04"))
	}
	line("")

	line("Incidents: %d", len(r.Incidents))
	for _, i := range r.Incidents {
		fix := "still open"
		if i.ToFixS != nil {
			fix = "fixed in " + dur(*i.ToFixS)
		}
		line("  %s  %-8s %s (%s)", i.Opened.In(loc).Format("Mon Jan 2 15:04"), i.Severity, i.Title, fix)
		if i.Note != nil {
			line("      cause: %s", i.Note.Text)
		}
	}
	line("")

	// Backups.
	exp, cur := 0, 0
	var probs []string
	for _, s := range r.Backups {
		exp += s.Expected
		cur += s.Current
		probs = append(probs, s.Problems...)
	}
	if len(r.Backups) > 0 {
		if len(probs) == 0 {
			line("Backups: all %d current on every host, every job succeeded, disks healthy.", exp)
		} else {
			line("Backups: %d of %d current.", cur, exp)
			for _, p := range probs {
				line("  - %s", p)
			}
		}
		line("")
	}

	// Certificates within 30 days, disks within 90.
	var certs, disks []string
	for _, c := range r.Certs {
		if c.Days < 30 {
			certs = append(certs, fmt.Sprintf("%s (%s) in %s", c.Name, c.Where, plural(c.Days, "day", "days")))
		}
	}
	for _, d := range r.Disks {
		if d.FullDays != nil && *d.FullDays < 90 {
			disks = append(disks, fmt.Sprintf("%s %s is %.0f%% full, full around %s", d.Host, d.Mount, d.Pct, d.Projected))
		}
	}
	if len(certs) == 0 {
		line("Certificates: none expire in the next 30 days.")
	} else {
		line("Certificates expiring within 30 days:")
		for _, c := range certs {
			line("  - %s", c)
		}
	}
	if len(disks) == 0 {
		line("Disks: none fill up in the next 90 days at this week's rate.")
	} else {
		line("Disks filling within 90 days:")
		for _, d := range disks {
			line("  - %s", d)
		}
	}
	if m := r.Mail; m != nil && m.Rounds > 0 {
		listed := 0
		for _, l := range m.Listed {
			if l.DegradedS+l.DownS > 0 {
				listed++
			}
		}
		bl := "every mail IP stayed off the block lists"
		if listed > 0 {
			bl = fmt.Sprintf("%s listed at some point", plural(listed, "block-list check was", "block-list checks were"))
		}
		line("Mail: %d test messages, %d missed; typically %.1f s, slowest %.1f s. The queue peaked at %d. %s.",
			m.Rounds, m.Missed, m.MedianS, m.MaxS, m.QueueMax, strings.ToUpper(bl[:1])+bl[1:])
	}
	line("")
	line("The full report: %s/reports?period=7d", srv.cfg.Alerts.PublicURL)
	text = b.String()

	short = fmt.Sprintf("%s. %s this week", strings.ToUpper(verdict[:1])+verdict[1:], plural(len(r.Incidents), "incident", "incidents"))
	if len(r.Backups) > 0 {
		short += fmt.Sprintf("; backups %d of %d current", cur, exp)
	}
	short += "."
	return subject, text, short, nil
}

func pctText(p float64) string {
	if p >= 99.995 {
		return "100%"
	}
	return strconv.FormatFloat(math2(p), 'f', 2, 64) + "%"
}

func math2(p float64) float64 { return float64(int(p*100)) / 100 } // round down: 99.999 isn't 100

func dur(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0f s", s)
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%.0f days", d.Hours()/24)
}

// --- HTTP ---

func (srv *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from, to, label, err := period(r.URL.Query().Get("period"), now, srv.reportsLoc())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rep, err := srv.buildReport(from, to, now, label)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "error": err.Error()})
		return
	}
	// The months there's history for, newest first.
	months := []string{}
	if start := rep.HistorySince; !start.IsZero() {
		loc := srv.reportsLoc()
		m := time.Date(start.In(loc).Year(), start.In(loc).Month(), 1, 0, 0, 0, 0, loc)
		for ; !m.After(now); m = m.AddDate(0, 1, 0) {
			months = append([]string{m.Format("2006-01")}, months...)
		}
	}
	var digest map[string]any
	if c := srv.cfg.Reports; c != nil && c.Digest != nil {
		var to []string
		for _, ch := range srv.digestChannels() {
			to = append(to, ch.ID())
		}
		digest = map[string]any{"weekday": c.Digest.Weekday, "at": c.Digest.At, "timezone": c.Timezone, "channels": to, "last": srv.db.Meta("digest_last")}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "report": rep, "months": months, "digest": digest, "timezone": srv.reportsLoc().String()})
}

// handleReportCSV sends one section of a report as CSV.
func (srv *Server) handleReportCSV(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	p := r.URL.Query().Get("period")
	from, to, _, err := period(p, now, srv.reportsLoc())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := srv.buildReport(from, to, now, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	loc := srv.reportsLoc()
	ts := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	var rows [][]string
	section := r.PathValue("section")
	switch section {
	case "services", "hosts", "checks":
		list := map[string][]Uptime{"services": rep.Services, "hosts": rep.Hosts, "checks": rep.Checks}[section]
		rows = append(rows, []string{"id", "name", "uptime_pct", "minutes_down", "minutes_degraded", "minutes_watched", "incidents"})
		for _, u := range list {
			rows = append(rows, []string{u.ID, u.Name, f(u.Pct), f(u.DownS / 60), f(u.DegradedS / 60), f(u.KnownS / 60), strconv.Itoa(u.Incidents)})
		}
	case "incidents":
		rows = append(rows, []string{"id", "opened", "severity", "title", "host", "service", "acknowledged_by", "minutes_to_ack", "resolved", "minutes_to_fix", "note"})
		for _, i := range rep.Incidents {
			ack, fix, res, note := "", "", "", ""
			if i.ToAckS != nil {
				ack = f(*i.ToAckS / 60)
			}
			if i.ToFixS != nil {
				fix = f(*i.ToFixS / 60)
			}
			if i.Resolved != nil {
				res = ts(*i.Resolved)
			}
			if i.Note != nil {
				note = i.Note.Text
			}
			rows = append(rows, []string{i.ID, ts(i.Opened), i.Severity, i.Title, i.Host, i.Service, i.AckBy, ack, res, fix, note})
		}
	case "certificates":
		rows = append(rows, []string{"name", "where", "source", "expires", "days_left"})
		for _, c := range rep.Certs {
			rows = append(rows, []string{c.Name, c.Where, c.Source, ts(c.Expires), strconv.Itoa(c.Days)})
		}
	case "disks":
		rows = append(rows, []string{"host", "mount", "used_bytes", "total_bytes", "used_pct", "bytes_per_day", "full_on"})
		for _, d := range rep.Disks {
			rows = append(rows, []string{d.Host, d.Mount, strconv.FormatUint(d.Used, 10), strconv.FormatUint(d.Total, 10), f(d.Pct), f(d.PerDay), d.Projected})
		}
	case "mail":
		rounds, err := srv.db.MailRounds(from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows = append(rows, []string{"at", "seconds", "queue"})
		for _, m := range rounds {
			rows = append(rows, []string{ts(m.At), f(m.Seconds), strconv.Itoa(m.Queue)})
		}
	default:
		http.Error(w, "section is services, hosts, checks, incidents, certificates, disks or mail", http.StatusNotFound)
		return
	}
	name := "cairnobs-" + section + "-" + strings.NewReplacer(" ", "").Replace(map[bool]string{true: p, false: "30d"}[p != ""]) + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(rows)
}

// handleNote saves the cause someone wrote for an incident.
func (srv *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	if srv.db == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	text := strings.TrimSpace(body.Text)
	if len(text) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "keep it under 2000 characters"})
		return
	}
	id := r.PathValue("id")
	_, sess, _ := srv.sessions.get(r)
	who := sess.User
	if err := srv.db.SetNote(id, state.Note{Text: text, By: who, At: time.Now()}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(who, "wrote the cause of incident "+id, text)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDigest shows (GET) or sends now (POST) the weekly digest.
func (srv *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	if srv.cfg.Reports == nil || srv.cfg.Reports.Digest == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no digest in obs-beacond.yaml"})
		return
	}
	now := time.Now()
	if r.Method == http.MethodPost {
		if err := srv.sendDigest(r.Context(), now); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		_, sess, _ := srv.sessions.get(r)
		srv.audit(sess.User, "sent the weekly digest by hand", "")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	subject, text, short, err := srv.digest(now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var to []string
	for _, ch := range srv.digestChannels() {
		to = append(to, ch.ID())
	}
	sort.Strings(to)
	writeJSON(w, http.StatusOK, map[string]any{"subject": subject, "text": text, "short": short, "channels": to})
}
