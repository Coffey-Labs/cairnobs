package server

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/akamai"
	"github.com/cairnobs/cairnobs/internal/certinfo"
)

// akamaiPending is how long a GTM change may take to propagate before
// it's said to be stuck.
const akamaiPending = 30 * time.Minute

// akamaiConditions judges the latest Akamai reading (docs/akamai.md,
// Judging), as Cloudflare's are.
func (srv *Server) akamaiConditions(now time.Time) []Condition {
	if srv.ak == nil {
		return nil
	}
	s, cfg := srv.ak.Snapshot(), srv.ak.Config()
	const href = "/akamai"
	var out []Condition
	if s.Error != "" && now.Sub(s.ErrorSince) >= 3*cfg.Every {
		out = append(out, Condition{Key: "akamai/api", Severity: "warning", Title: "Can't read Akamai",
			Detail: s.Error + ". Traffic management checks are on hold; the last reading stays on screen.", Href: href})
	}
	if s.Checked.IsZero() {
		return out
	}
	hostOf := srv.hostsByAddr()
	for _, d := range s.Domains {
		switch {
		case d.Propagation == "DENIED":
			out = append(out, Condition{Key: "akamai/propagation/" + d.Name, Severity: "warning",
				Title: "Akamai refused the last change to " + d.Name, Detail: strings.TrimSuffix(d.Message, ".") + ".", Href: href})
		case d.Propagation == "PENDING" && !d.Since.IsZero() && now.Sub(d.Since) > akamaiPending:
			out = append(out, Condition{Key: "akamai/propagation/" + d.Name, Severity: "warning",
				Title:  "A change to " + d.Name + " has been propagating for " + roughly(now.Sub(d.Since)),
				Detail: "GTM changes usually reach every nameserver within minutes; until then some answer with the old configuration.", Href: href})
		}
		for _, p := range d.Properties {
			if p.At.IsZero() {
				continue // no availability report yet
			}
			name := p.Name + "." + d.Name
			var down []string
			enabled, live := 0, 0
			for _, t := range p.Targets {
				if !t.Enabled || len(t.Servers) == 0 {
					continue
				}
				enabled++
				if t.Live() {
					live++
				}
				for _, sv := range t.Servers {
					if !sv.Known || sv.Alive {
						continue
					}
					down = append(down, t.Datacenter+" ("+sv.IP+")")
					out = append(out, Condition{Key: "akamai/server/" + name + "/" + sv.IP, Severity: "warning",
						Title:  fmt.Sprintf("%s in %s is failing GTM's liveness test for %s", sv.IP, t.Datacenter, name),
						Detail: "Akamai stops handing it out and sends its share to the other datacenters.", Host: hostOf[sv.IP], Href: href})
				}
			}
			if enabled > 0 && live == 0 {
				out = append(out, Condition{Key: "akamai/property/" + name, Severity: "critical",
					Title:  "Akamai GTM: " + name + " has no live datacenter",
					Detail: "Every datacenter's servers are failing GTM's liveness tests: " + strings.Join(down, ", ") + ".", Href: href})
			}
		}
	}
	for _, e := range s.Enrollments {
		key := "akamai/cps/" + strconv.Itoa(e.ID)
		for _, c := range e.Deployed {
			if st, word := certStatus(c, now, srv.certCritical()); st != "good" {
				out = append(out, Condition{Key: key + "/" + c.Fingerprint, Severity: st, Ends: c.NotAfter,
					Title:  "Akamai certificate for " + e.CN + ": " + lowerFirst(word),
					Detail: akamaiEnrollment(e) + ", " + c.Key + ". " + akamaiRenewal(e), Href: href})
			}
		}
		if what := akamaiWaiting[e.Waiting]; what != "" {
			out = append(out, Condition{Key: key + "/waiting", Severity: "warning",
				Title:  "Akamai certificate for " + e.CN + ": the renewal is waiting on you",
				Detail: "CPS needs " + what + " before it can go on (" + akamaiEnrollment(e) + ").", Href: href})
		}
	}
	for _, p := range s.PMProperties {
		for _, n := range []struct {
			name string
			a    *akamai.Activation
			now  int
		}{{"staging", p.LastStaging, p.Staging}, {"production", p.LastProduction, p.Production}} {
			a := n.a
			if a == nil {
				continue
			}
			verb := "Activating"
			if a.Type == "DEACTIVATE" {
				verb = "Deactivating"
			}
			what := fmt.Sprintf("%s %s v%d on %s", verb, p.Name, a.Version, n.name)
			stays := "nothing is active there"
			if n.now > 0 {
				stays = fmt.Sprintf("v%d stays active", n.now)
			}
			note := ""
			if a.Note != "" {
				note = " Its note: " + strings.TrimSuffix(a.Note, ".") + "."
			}
			key := "akamai/activation/" + p.ID + "/" + n.name
			switch {
			case a.Status == "FAILED":
				out = append(out, Condition{Key: key, Severity: "warning", Title: "Akamai: " + lowerFirst(what) + " failed",
					Detail: fmt.Sprintf("Submitted %s; %s.%s", a.Submitted.UTC().Format("2006-01-02 15:04 UTC"), stays, note), Href: href})
			case a.Pending() && now.Sub(a.Submitted) > akamaiActivationPending:
				out = append(out, Condition{Key: key, Severity: "warning", Title: "Akamai: " + lowerFirst(what) + " has been pending for " + roughly(now.Sub(a.Submitted)),
					Detail: fmt.Sprintf("Activations usually finish well within an hour (it's at %s); until it does, %s.%s", a.Status, stays, note), Href: href})
			}
		}
	}
	for _, t := range s.Traffic {
		if t.Errors < int64(cfg.ErrorMin) {
			continue
		}
		sev := ""
		switch pct := t.Pct(); {
		case pct >= cfg.ErrorCriticalPct:
			sev = "critical"
		case pct >= cfg.ErrorWarnPct:
			sev = "warning"
		}
		if sev != "" {
			out = append(out, Condition{Key: fmt.Sprintf("akamai/traffic/%d", t.CPCode), Severity: sev,
				Title: fmt.Sprintf("Akamai: %.0f%% of CP code %d's requests are failing with 5xx", t.Pct(), t.CPCode),
				Detail: fmt.Sprintf("%d of %d edge hits between %s and %s UTC (the latest 5 minutes Akamai has reported). Usually the origin behind it; the GTM and Property Manager panels say whether Akamai changed anything.",
					t.Errors, t.Hits, s.TrafficStart.UTC().Format("15:04"), s.TrafficEnd.UTC().Format("15:04")), Href: href})
		}
	}
	for _, z := range s.DNSZones {
		if added, removed := srv.akdns.diff(z.Name, akamaiRecords(z.Records)); len(added)+len(removed) > 0 {
			var lines []string
			for _, r := range removed {
				lines = append(lines, "removed "+r)
			}
			for _, r := range added {
				lines = append(lines, "added "+r)
			}
			if len(lines) > 4 {
				lines = append(lines[:3], fmt.Sprintf("and %d more", len(lines)-3))
			}
			out = append(out, Condition{Key: "akamai/dns/" + z.Name, Severity: "warning",
				Title:  fmt.Sprintf("Akamai Edge DNS for %s changed (%d added, %d removed)", z.Name, len(added), len(removed)),
				Detail: strings.Join(lines, "; ") + ". Accept it on the Akamai page if it was meant.", Href: href})
		}
	}
	return out
}

// akamaiRecords are a zone's record sets as a DNS baseline holds them;
// nil when they weren't read.
func akamaiRecords(recs []akamai.RecordSet) map[string]string {
	if recs == nil {
		return nil
	}
	m := make(map[string]string, len(recs))
	for _, r := range recs {
		m[r.Key()] = r.String()
	}
	return m
}

func (srv *Server) handleAkamaiDNSAccept(w http.ResponseWriter, r *http.Request) {
	if srv.ak == nil {
		http.NotFound(w, r)
		return
	}
	zone := r.PathValue("zone")
	for _, z := range srv.ak.Snapshot().DNSZones {
		if z.Name != zone || z.Records == nil {
			continue
		}
		added, removed := srv.akdns.accept(zone, akamaiRecords(z.Records))
		who := "someone"
		if _, s, ok := srv.sessions.get(r); ok {
			who = s.User
		}
		srv.audit(who, "accepted Akamai DNS changes", fmt.Sprintf("%s: %d added, %d removed", zone, added, removed))
		writeJSON(w, http.StatusOK, map[string]int{"added": added, "removed": removed})
		return
	}
	http.NotFound(w, r)
}

// akamaiActivationPending is how long a Property Manager activation may
// take before it's said to be stuck.
const akamaiActivationPending = time.Hour

// akamaiWaiting says what each kind of input a CPS change can be waiting
// for is, in words.
var akamaiWaiting = map[string]string{
	"third-party-csr":                            "the CSR signed by your certificate authority and uploaded",
	"third-party-certificate":                    "the certificate your authority issued, uploaded",
	"change-management-info":                     "the certificate on staging checked and acknowledged",
	"lets-encrypt-challenges":                    "the domain validation challenges answered",
	"pre-verification-warnings-acknowledgement":  "its warnings acknowledged",
	"post-verification-warnings-acknowledgement": "its warnings acknowledged",
}

// akamaiEnrollment names an enrollment: "CPS enrollment 1002 (shop.example.com)".
func akamaiEnrollment(e akamai.Enrollment) string {
	return "CPS enrollment " + strconv.Itoa(e.ID) + " (" + e.CN + ")"
}

// akamaiRenewal says who renews it.
func akamaiRenewal(e akamai.Enrollment) string {
	switch {
	case e.Validation == "third-party":
		return "A third-party certificate: renewing it is up to you, in Control Center → Certificate Provisioning System."
	case e.Pending > 0:
		return "A renewal is in progress in CPS."
	}
	return "CPS starts renewing its own certificates well before they end, so one this close means a renewal stalled: look in Control Center → Certificate Provisioning System."
}

// roughly is a duration in words: "45 minutes", "3 hours".
func roughly(d time.Duration) string {
	if d < 2*time.Hour {
		return plural(int(d.Minutes()), "minute", "minutes")
	}
	return plural(int(d.Hours()), "hour", "hours")
}

// hostsByAddr maps each address a host's map sample has on an interface
// to the host, so a server Akamai names is the host it is.
func (srv *Server) hostsByAddr() map[string]string {
	m := map[string]string{}
	for _, h := range srv.store.Snapshot() {
		if h.mp == nil {
			continue
		}
		for _, i := range h.mp.Interfaces {
			for _, a := range i.Addrs {
				if ip, _, err := net.ParseCIDR(a); err == nil {
					m[ip.String()] = h.name
				} else if ip := net.ParseIP(a); ip != nil {
					m[ip.String()] = h.name
				}
			}
		}
	}
	return m
}

func (srv *Server) handleAkamai(w http.ResponseWriter, r *http.Request) {
	if srv.ak == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	snap, cfg := srv.ak.Snapshot(), srv.ak.Config()
	hostOf := srv.hostsByAddr()
	hosts := map[string]string{}
	for _, d := range snap.Domains {
		for _, p := range d.Properties {
			for _, t := range p.Targets {
				for _, s := range t.Servers {
					if h := hostOf[s.IP]; h != "" {
						hosts[s.IP] = h
					}
				}
			}
		}
	}
	// Each deployed certificate judged as on the Certificates page.
	type judged struct {
		Status string `json:"status"`
		Word   string `json:"word"`
		Weak   string `json:"weak,omitempty"`
	}
	certs := map[string]judged{}
	now := time.Now()
	for _, e := range snap.Enrollments {
		for _, c := range e.Deployed {
			st, word := certStatus(c, now, srv.certCritical())
			certs[c.Fingerprint] = judged{st, word, certinfo.Weak(&c)}
		}
	}
	changes := map[string]map[string][]string{}
	for _, z := range snap.DNSZones {
		if added, removed := srv.akdns.diff(z.Name, akamaiRecords(z.Records)); len(added)+len(removed) > 0 {
			changes[z.Name] = map[string][]string{"added": nonNilStrings(added), "removed": nonNilStrings(removed)}
		}
	}
	// Record sets aren't sent: thousands, and the changes are what matters.
	// (A copy: the watcher's reading shares its slices.)
	snap.DNSZones = slices.Clone(snap.DNSZones)
	for i := range snap.DNSZones {
		snap.DNSZones[i].Count = len(snap.DNSZones[i].Records)
		snap.DNSZones[i].Records = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "snapshot": snap, "hosts": hosts, "certs": certs, "dns_changes": changes, "every_s": int(cfg.Every / time.Second),
		"error_warn_pct": cfg.ErrorWarnPct, "error_critical_pct": cfg.ErrorCriticalPct, "error_min": cfg.ErrorMin,
		"conditions": nonNilConds(srv.akamaiConditions(now)),
	})
}

// akamaiReading is how the evaluator's reading is shared.
type akamaiReading = akamai.Snapshot
