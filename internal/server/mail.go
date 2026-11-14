package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// mailConditions judges the mail monitor's latest round. One bad round
// warns at most (a reboot or a blip); two in a row are an outage.
func (srv *Server) mailConditions(now time.Time) []Condition {
	if srv.mail == nil {
		return nil
	}
	s, cfg := srv.mail.Snapshot(), srv.mail.Config()
	const href = "/mail"
	cond := func(key, sev, title, detail string) Condition {
		return Condition{Key: "mail/" + key, Severity: sev, Title: title, Detail: detail, Host: cfg.Host, Href: href}
	}
	var out []Condition
	if s.Error != "" {
		out = append(out, cond("monitor", "warning", "The mail monitor can't run", s.Error+"."))
	}
	if s.Checked.IsZero() {
		return out
	}
	if now.Sub(s.Checked) > 3*cfg.Every {
		out = append(out, cond("stale", "warning", "The mail monitor has stopped",
			fmt.Sprintf("Its last round was %s ago.", now.Sub(s.Checked).Round(time.Minute))))
	}
	jmapDown := false
	for _, si := range s.SignIns {
		if si.Fails == 0 {
			continue
		}
		if si.Name == "JMAP" {
			jmapDown = true
		}
		if si.Fails >= 2 {
			out = append(out, cond("signin/"+strings.ToLower(si.Name), "critical",
				"Can't sign in to mail over "+si.Name,
				fmt.Sprintf("%s: %s (the monitor account, %d rounds in a row).", cfg.Account, si.Error, si.Fails)))
		}
	}
	// Without JMAP there's no telling whether mail arrived or what's queued;
	// the sign-in condition says so.
	if t := s.Trip; !jmapDown {
		switch {
		case !t.OK && t.Fails >= 2:
			out = append(out, cond("roundtrip", "critical", "Test mail isn't arriving",
				fmt.Sprintf("%s, %d rounds in a row. Mail may be stuck between submission and the mailbox.", t.Error, t.Fails)))
		case !t.OK:
			out = append(out, cond("roundtrip", "warning", "A test message didn't arrive", t.Error+"."))
		case time.Duration(t.Seconds*float64(time.Second)) > cfg.RoundTripWarn:
			out = append(out, cond("roundtrip", "warning", "Mail is slow",
				fmt.Sprintf("A test message took %s to arrive; usually it's a second or two.", seconds(t.Seconds))))
		}
	}
	if q := s.Queue; q.Read {
		detail := fmt.Sprintf("%d messages waiting to go out", q.Total)
		if q.Oldest != nil {
			detail += fmt.Sprintf(", the oldest for %s", now.Sub(*q.Oldest).Round(time.Minute))
		}
		detail += ". A remote server refusing us, a block list, or delivery stuck."
		switch {
		case q.Total >= cfg.QueueCritical:
			out = append(out, cond("queue", "critical", fmt.Sprintf("%d messages in the mail queue", q.Total), detail))
		case q.Total >= cfg.QueueWarn:
			out = append(out, cond("queue", "warning", fmt.Sprintf("%d messages in the mail queue", q.Total), detail))
		}
	} else if !jmapDown && q.Fails >= 2 {
		out = append(out, cond("queue", "warning", "Can't read the mail queue", q.Error+"."))
	}
	return out
}

func seconds(s float64) string {
	if s < 90 {
		return fmt.Sprintf("%.0f seconds", s)
	}
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

func (srv *Server) handleMail(w http.ResponseWriter, r *http.Request) {
	if srv.mail == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	now := time.Now()
	s, cfg := srv.mail.Snapshot(), srv.mail.Config()
	// The mail service's own checks (ports, block lists), from the service map.
	checks := []CheckView{}
	if t := srv.currentTopology(); t != nil {
		var ids []string
		for _, svc := range t.Services {
			if svc.ID == "mail" {
				ids = svc.Checks
			}
		}
		views, _ := srv.judgeChecks(now)
		for _, v := range views {
			if slices.Contains(ids, v.ID) {
				checks = append(checks, v)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "account": cfg.Account, "server": cfg.Server,
		"every_s": int(cfg.Every / time.Second), "wait_s": int(cfg.Wait / time.Second),
		"round_trip_warn_s": cfg.RoundTripWarn.Seconds(), "queue_warn": cfg.QueueWarn, "queue_critical": cfg.QueueCritical,
		"snapshot": s, "conditions": nonNilConds(srv.mailConditions(now)), "checks": checks,
	})
}
