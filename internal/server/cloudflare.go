package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/state"
)

// cloudflareConditions judges the latest Cloudflare reading. The status
// page is shown but never alerts: a Cloudflare incident somewhere in the
// world usually isn't ours, and when it is, the pools say so.
func (srv *Server) cloudflareConditions(now time.Time) []Condition {
	if srv.cf == nil {
		return nil
	}
	s, cfg := srv.cf.Snapshot(), srv.cf.Config()
	const href = "/cloudflare"
	var out []Condition
	if s.Error != "" && now.Sub(s.ErrorSince) >= 3*cfg.Every {
		out = append(out, Condition{Key: "cloudflare/api", Severity: "warning", Title: "Can't read Cloudflare",
			Detail: s.Error + ". Load balancer, SSL and certificate checks are on hold.", Href: href})
	}
	if s.Checked.IsZero() {
		return out
	}
	for _, p := range s.Pools {
		if !p.Enabled {
			continue
		}
		if !p.Healthy {
			out = append(out, Condition{Key: "cloudflare/pool/" + p.Name, Severity: "critical",
				Title:  "Cloudflare pool " + p.Name + " has no healthy origin",
				Detail: "Every origin is failing Cloudflare's health check, so the sites behind it are down or on a fallback.", Href: href})
		}
		for _, o := range p.Origins {
			if !o.Enabled || o.Healthy {
				continue
			}
			detail := o.Reason
			if o.Code != 0 {
				detail += fmt.Sprintf(", HTTP %d", o.Code)
			}
			out = append(out, Condition{Key: "cloudflare/origin/" + p.Name + "/" + o.Name, Severity: "warning",
				Title:  fmt.Sprintf("%s is failing %s's health check", o.Name, p.Name),
				Detail: detail + ". Cloudflare sends its share of traffic to the other origins.", Host: o.Name, Href: href})
		}
	}
	for _, z := range s.Zones {
		if t := z.Traffic; t != nil && t.Requests > 0 {
			pct := 100 * float64(t.Errors) / float64(t.Requests)
			sev := ""
			switch {
			case t.Errors >= cfg.ErrorMin && pct >= cfg.ErrorCriticalPct:
				sev = "critical"
			case t.Errors >= cfg.ErrorMin && pct >= cfg.ErrorWarnPct:
				sev = "warning"
			}
			if sev != "" {
				detail := fmt.Sprintf("%d of %d requests in the last 15 minutes were answered with a server error", t.Errors, t.Requests)
				if len(t.ErrorHosts) > 0 {
					detail += fmt.Sprintf(", most on %s (%d)", t.ErrorHosts[0].Host, t.ErrorHosts[0].Count)
				}
				out = append(out, Condition{Key: "cloudflare/errors/" + z.Name, Severity: sev,
					Title: fmt.Sprintf("%s: %.0f%% of requests failing", z.Name, pct), Detail: detail + ".", Href: href})
			}
			usual := float64(t.BlockedDay) / 96 // the day before, per 15 minutes
			if t.Blocked >= cfg.BlockMin && float64(t.Blocked) >= cfg.BlockFactor*usual {
				out = append(out, Condition{Key: "cloudflare/blocks/" + z.Name, Severity: "warning",
					Title:  fmt.Sprintf("Cloudflare is blocking a lot of %s traffic", z.Name),
					Detail: fmt.Sprintf("%d requests blocked or challenged in the last 15 minutes; usually about %.0f. An attack, a bot, or a rule catching real visitors.", t.Blocked, usual), Href: href})
			}
		}
		if added, removed := srv.dns.diff(z.Name, records(z.Records)); len(added)+len(removed) > 0 {
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
			out = append(out, Condition{Key: "cloudflare/dns/" + z.Name, Severity: "warning",
				Title:  fmt.Sprintf("DNS for %s changed (%d added, %d removed)", z.Name, len(added), len(removed)),
				Detail: strings.Join(lines, "; ") + ". Accept it on the Cloudflare page if it was meant.", Href: href})
		}
		if want := cfg.WantSSL(z.Name); z.SSL != "" && z.SSL != want {
			out = append(out, Condition{Key: "cloudflare/ssl/" + z.Name, Severity: "warning",
				Title:  fmt.Sprintf("%s SSL mode is %s, not %s", z.Name, z.SSL, want),
				Detail: "Cloudflare's SSL/TLS encryption mode for the zone changed.", Href: href})
		}
		for _, c := range z.Certs {
			key := "cloudflare/cert/" + z.Name + "/" + strings.Join(c.Hosts, ",")
			days := int(c.Expires.Sub(now).Hours() / 24)
			switch {
			case c.Status != "active":
				out = append(out, Condition{Key: key, Severity: "warning",
					Title:  fmt.Sprintf("%s edge certificate is %s", z.Name, strings.ReplaceAll(c.Status, "_", " ")),
					Detail: "Covers " + strings.Join(c.Hosts, ", ") + ".", Href: href})
			case c.Expires.IsZero():
			case days < cfg.CertCriticalDays:
				out = append(out, Condition{Key: key, Severity: "critical", Ends: c.Expires,
					Title:  fmt.Sprintf("%s edge certificate expires in %s", z.Name, plural(days, "day", "days")),
					Detail: "Cloudflare normally renews well before this; renewal is stuck. Covers " + strings.Join(c.Hosts, ", ") + ".", Href: href})
			case days < cfg.CertWarnDays:
				out = append(out, Condition{Key: key, Severity: "warning", Ends: c.Expires,
					Title:  fmt.Sprintf("%s edge certificate expires in %s", z.Name, plural(days, "day", "days")),
					Detail: "Cloudflare normally renews 30 days ahead. Covers " + strings.Join(c.Hosts, ", ") + ".", Href: href})
			}
		}
	}
	return out
}

func (srv *Server) handleCloudflare(w http.ResponseWriter, r *http.Request) {
	if srv.cf == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	cfg := srv.cf.Config()
	snap := srv.cf.Snapshot()
	changes := map[string]map[string][]string{}
	for _, z := range snap.Zones {
		if added, removed := srv.dns.diff(z.Name, records(z.Records)); len(added)+len(removed) > 0 {
			changes[z.Name] = map[string][]string{"added": nonNilStrings(added), "removed": nonNilStrings(removed)}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "snapshot": snap, "dns_changes": changes,
		"error_warn_pct": cfg.ErrorWarnPct, "error_critical_pct": cfg.ErrorCriticalPct,
		"conditions": nonNilConds(srv.cloudflareConditions(time.Now())),
		"ssl_mode":   cfg.SSLMode, "ssl_mode_zones": cfg.SSLModeZones, "cert_warn_days": cfg.CertWarnDays, "cert_critical_days": cfg.CertCriticalDays,
		"every_s": int(cfg.Every / time.Second),
	})
}

func nonNilConds(c []Condition) []Condition {
	if c == nil {
		return []Condition{}
	}
	return c
}

func (srv *Server) handleDNSAccept(w http.ResponseWriter, r *http.Request) {
	if srv.cf == nil {
		http.NotFound(w, r)
		return
	}
	zone := r.PathValue("zone")
	for _, z := range srv.cf.Snapshot().Zones {
		if z.Name != zone {
			continue
		}
		added, removed := srv.dns.accept(zone, records(z.Records))
		who := "someone"
		if _, s, ok := srv.sessions.get(r); ok {
			who = s.User
		}
		srv.audit(who, "accepted DNS changes", fmt.Sprintf("%s: %d added, %d removed", zone, added, removed))
		writeJSON(w, http.StatusOK, map[string]int{"added": added, "removed": removed})
		return
	}
	http.NotFound(w, r)
}

// dnsBaseline holds each zone's DNS records as last accepted, by Record.Key.
// A zone seen for the first time is taken as it is. With a database the
// baseline survives restarts, so a change made while obs-beacond was down is
// still caught.
// Akamai's Edge DNS zones have one of their own (prefix "ak_dns:").
type dnsBaseline struct {
	mu     sync.Mutex
	m      map[string]map[string]string
	db     *state.DB
	prefix string // the database key's; "cf_dns:" when empty
}

// records are Cloudflare's records as a baseline holds them: each one's
// whole setting to how it reads; nil when they were never read.
func records(recs []cloudflare.Record) map[string]string {
	if recs == nil {
		return nil
	}
	m := make(map[string]string, len(recs))
	for _, r := range recs {
		m[r.Key()] = r.String()
	}
	return m
}

func (b *dnsBaseline) key(zone string) string {
	if b.prefix == "" {
		return "cf_dns:" + zone
	}
	return b.prefix + zone
}

// base returns the zone's baseline, loading it or starting it from recs.
// Callers hold b.mu.
func (b *dnsBaseline) base(zone string, recs map[string]string) map[string]string {
	if b.m == nil {
		b.m = map[string]map[string]string{}
	}
	if m, ok := b.m[zone]; ok {
		return m
	}
	if b.db != nil {
		if v := b.db.Meta(b.key(zone)); v != "" {
			var m map[string]string
			if json.Unmarshal([]byte(v), &m) == nil {
				b.m[zone] = m
				return m
			}
		}
	}
	b.set(zone, recs)
	return recs
}

func (b *dnsBaseline) set(zone string, m map[string]string) {
	b.m[zone] = m
	if b.db != nil {
		v, _ := json.Marshal(m)
		if err := b.db.SetMeta(b.key(zone), string(v)); err != nil {
			log.Printf("can't store the DNS baseline for %s: %v", zone, err)
		}
	}
}

// diff lists what was added and removed since the baseline, readable and sorted.
func (b *dnsBaseline) diff(zone string, cur map[string]string) (added, removed []string) {
	if cur == nil {
		return nil, nil // never read: nothing to compare
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	base := b.base(zone, cur)
	for k, v := range cur {
		if _, ok := base[k]; !ok {
			added = append(added, v)
		}
	}
	for k, v := range base {
		if _, ok := cur[k]; !ok {
			removed = append(removed, v)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// accept makes the current records the baseline and says what that took in.
func (b *dnsBaseline) accept(zone string, cur map[string]string) (added, removed int) {
	if cur == nil {
		return 0, 0 // never read: nothing to accept
	}
	a, r := b.diff(zone, cur)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.set(zone, cur)
	return len(a), len(r)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
