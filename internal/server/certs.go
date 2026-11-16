package server

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/certfiles"
	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/tlsscan"
)

// The certificate inventory's page (docs/cert-inventory.md, step 1): every
// certificate obs-beacond knows of, one row per certificate (by its
// SHA-256 fingerprint) with every place it was seen. Sources so far:
// `cert` records (Vault's PKI), site checks' handshakes and Cloudflare's
// edge certificates. Alerts stay
// with each source (decision 5); this page only gathers.

const (
	certWarn   = 21 * 24 * time.Hour
	certPlaces = 50 // places sent per certificate; PlaceCount has them all
	// defaultCertCritical is certificates.critical_within's default: four
	// days left is a job for working hours, two is not (#34).
	defaultCertCritical = 48 * time.Hour
)

// CertificatesConfig is obs-beacond.yaml's certificates section.
type CertificatesConfig struct {
	// CriticalWithin is how close to its end a certificate becomes
	// critical (48h). Before that it's a warning, from whenever its source
	// starts warning (21 days for most).
	CriticalWithin time.Duration `yaml:"critical_within"`
}

func (srv *Server) certCritical() time.Duration {
	if srv.cfg == nil || srv.cfg.Certificates.CriticalWithin <= 0 {
		return defaultCertCritical
	}
	return srv.cfg.Certificates.CriticalWithin
}

// certSeverity holds every certificate condition at warning until its
// certificate is within critical of its end, whatever its source judged
// (each has its own critical threshold: 7 days for checks and Vault, 3
// for Cloudflare). A source's critical inside the window stands, and so
// does one about something other than the end (an ended certificate is
// past it).
func certSeverity(conds []Condition, now time.Time, critical time.Duration) {
	for i := range conds {
		c := &conds[i]
		if c.Severity == "critical" && !c.Ends.IsZero() && c.Ends.Sub(now) > critical {
			c.Severity = "warning"
		}
	}
}

// CertInvPlace is where a certificate was seen.
type CertInvPlace struct {
	Kind  string `json:"kind"`  // "host", "file", "store", "vault", "check", "cloudflare", "cluster", "akamai"
	Where string `json:"where"` // "vault-prod (obs-beacond) · pki_int", "shop from web1"
	Href  string `json:"href"`
	Host  string `json:"host,omitempty"` // a host's place: which host
}

// CertInv is one certificate on the page.
type CertInv struct {
	report.Cert
	Days       int            `json:"days"`   // left; negative once ended
	Status     string         `json:"status"` // good, warning, critical
	Word       string         `json:"word"`
	Weak       string         `json:"weak,omitempty"`
	Renewals   int            `json:"renewals,omitempty"` // Vault: certificates its names have had
	Places     []CertInvPlace `json:"places"`
	PlaceCount int            `json:"place_count"`
	Kinds      []string       `json:"kinds"`
	// NotReloaded are the ports still serving it though a renewed one is
	// in the file on the same host (decision 4): "node1:443 · nginx".
	NotReloaded []string `json:"not_reloaded,omitempty"`
	// Renewed is that renewed one: its file and end.
	Renewed string `json:"renewed,omitempty"`
	// Wrong is what's wrong with how it's served, by place (step 5):
	// "node1:443 · nginx: sent without its intermediate certificate".
	Wrong []string `json:"wrong,omitempty"`
	// Reloads are the reloads an admin may ask for: a not-reloaded port
	// whose host's cert_reload lists a unit for the renewed file
	// (docs/cert-rotation.md, step 2).
	Reloads []CertReload `json:"reloads,omitempty"`
	// Renews are the cert-manager Certificates behind it an admin may ask
	// to renew now: their cluster agent runs with -cert-renew, and
	// cert-manager isn't renewing them already (step 3).
	Renews []CertRenew `json:"renews,omitempty"`
}

// CertRenew is one renewal on offer, and the last one asked for.
type CertRenew struct {
	Source      string      `json:"source"` // the cluster agent
	Namespace   string      `json:"namespace"`
	Certificate string      `json:"certificate"`
	Where       string      `json:"where"`
	Last        *ActionView `json:"last,omitempty"`
}

// CertReload is one reload on offer, and the last one asked for.
type CertReload struct {
	Host  string      `json:"host"`
	Unit  string      `json:"unit"`
	Addr  string      `json:"addr"`
	Port  int         `json:"port"`
	Where string      `json:"where"`
	File  string      `json:"file"` // the renewed file
	Last  *ActionView `json:"last,omitempty"`
}

// reloadGrace is how long a renewed file may wait for its service's
// reload before it's said not to have happened.
const reloadGrace = time.Hour

// certStatus judges a certificate by the time it has left: 21 days, and
// critical (certificates.critical_within), or a third and a tenth of a
// short life (the same as Vault's leaves).
func certStatus(c report.Cert, now time.Time, critical time.Duration) (string, string) {
	left, life := c.NotAfter.Sub(now), c.NotAfter.Sub(c.NotBefore)
	warn, crit := certWarn, critical
	if life > 0 && !c.CA {
		warn, crit = min(warn, life/3), min(crit, life/10)
	}
	date := c.NotAfter.Format("2006-01-02")
	switch {
	case left <= 0:
		return "critical", "Ended " + date
	case left < crit:
		return "critical", "Ends in " + certLeft(left) + " (" + date + ")"
	case left < warn:
		return "warning", "Ends in " + certLeft(left) + " (" + date + ")"
	}
	return "good", "Ends " + date
}

// certLeft is time left in days, or hours under two days.
func certLeft(d time.Duration) string {
	if d < 48*time.Hour {
		return strconv.Itoa(int(d.Hours())) + " hours"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}

// certificates gathers every certificate, soonest end first.
func (srv *Server) certificates(now time.Time) []*CertInv {
	rows := map[string]*CertInv{}
	add := func(c report.Cert, key string, p CertInvPlace) *CertInv {
		if c.Fingerprint != "" {
			key = c.Fingerprint
		}
		row := rows[key]
		if row == nil {
			row = &CertInv{Cert: c}
			rows[key] = row
		}
		row.PlaceCount++
		if len(row.Places) < certPlaces {
			row.Places = append(row.Places, p)
		}
		if !slices.Contains(row.Kinds, p.Kind) {
			row.Kinds = append(row.Kinds, p.Kind)
		}
		return row
	}
	for _, s := range srv.workloadSnapshot() {
		mayRenew := false // the cluster agent's own cluster, with -cert-renew
		for _, r := range s.recs {
			if r.Kind == report.KindCluster && r.ID == report.ClusterSelf && sourceAgent(s.name) == s.name {
				mayRenew, _ = r.Facts["cert_renew"].(bool)
			}
		}
		for _, r := range s.recs {
			if r.Kind != report.KindCert {
				continue
			}
			c := certOfRecord(r)
			kind := "cluster"
			if sourceAgent(s.name) == serverSource {
				kind = "vault"
			}
			where := s.title()
			if r.Namespace != "" {
				where += " · " + r.Namespace
			}
			if c, _ := r.Facts["certificate"].(string); c != "" { // cert-manager's (step 4)
				where += " · Certificate " + c
			}
			if on, _ := r.Facts["served_on"].(string); on != "" {
				where += " · served on " + on
			}
			row := add(c, "rec\x00"+s.name+"\x00"+r.ID, CertInvPlace{Kind: kind, Where: where, Href: workloadHref(s.name, r.Namespace, r.Name)})
			if cm, _ := r.Facts["certificate"].(string); cm != "" && mayRenew && kind == "cluster" {
				if issuing, _ := r.Facts["issuing"].(bool); !issuing {
					row.Renews = append(row.Renews, CertRenew{Source: s.name, Namespace: r.Namespace, Certificate: cm, Where: s.title() + " · " + r.Namespace + " · Certificate " + cm})
				}
			}
			if w, _ := r.Facts["wrong"].(string); w != "" { // a cluster's handshakes (step 5)
				for _, x := range strings.Split(w, "; ") {
					row.Wrong = append(row.Wrong, s.title()+" "+x)
				}
			}
			if n, ok := r.Facts["certificates"].(float64); ok && int(n) > row.Renewals {
				row.Renewals = int(n)
			} else if n, ok := r.Facts["certificates"].(int); ok && n > row.Renewals {
				row.Renewals = n
			}
		}
	}
	if cs := srv.currentChecks(); cs != nil {
		names := map[string]string{}
		for _, c := range cs.Checks {
			names[c.ID] = c.Name
		}
		for _, h := range srv.store.Snapshot() {
			if h.seen.IsZero() || now.Sub(h.seen) > srv.cfg.StaleAfter {
				continue
			}
			for _, res := range h.last.Checks {
				if res.Cert == nil || names[res.ID] == "" {
					continue
				}
				add(*res.Cert, "", CertInvPlace{Kind: "check", Where: names[res.ID] + " from " + h.name, Href: "/checks"})
			}
		}
	}
	// What hosts' ports serve (step 2): tried once a day, so a place
	// counts while it was seen in the last two days.
	var hostPorts []hostPort
	for _, h := range srv.store.Snapshot() {
		if h.certs == nil {
			continue
		}
		for _, c := range h.certs.List {
			if now.Sub(c.At) > 2*tlsscan.Every {
				continue
			}
			where := h.name + ":" + strconv.Itoa(c.Port)
			if c.Addr != "127.0.0.1" && c.Addr != "::1" {
				where = h.name + " " + net.JoinHostPort(c.Addr, strconv.Itoa(c.Port))
			}
			if c.How != "tls" {
				where += " (" + c.How + " STARTTLS)"
			}
			if c.Owner != "" {
				where += " · " + c.Owner
			}
			row := add(c.Cert, "", CertInvPlace{Kind: "host", Where: where, Href: "/hosts/" + h.name, Host: h.name})
			for _, w := range c.Wrong {
				row.Wrong = append(row.Wrong, where+": "+w)
			}
			hostPorts = append(hostPorts, hostPort{host: h.name, where: where, seen: c})
		}
	}
	// What hosts' files hold (step 3), and the ports still serving an
	// older certificate for the same names (decision 4).
	for _, h := range srv.store.Snapshot() {
		if h.files == nil || now.Sub(h.files.At) > 2*tlsscan.Every {
			continue
		}
		newest := map[string]report.CertFile{} // by the names it's for
		for _, f := range h.files.List {
			kind := "file"
			if strings.HasPrefix(f.Path, certfiles.StorePrefix) {
				kind = "store" // a Windows host's certificate store
			}
			add(f.Cert, "", CertInvPlace{Kind: kind, Where: h.name + ":" + f.Path, Href: "/hosts/" + h.name, Host: h.name})
			k := namesKey(f.Cert)
			if o, ok := newest[k]; !ok || f.Cert.NotAfter.After(o.Cert.NotAfter) {
				newest[k] = f
			}
		}
		for _, p := range hostPorts {
			f, ok := newest[namesKey(p.seen.Cert)]
			if p.host != h.name || !ok || f.Cert.Fingerprint == p.seen.Cert.Fingerprint || !f.Cert.NotAfter.After(p.seen.Cert.NotAfter) ||
				now.Sub(f.Mod) < reloadGrace || p.seen.At.Before(f.Mod.Add(reloadGrace)) {
				continue // the same, older, or just renewed (or not tried since)
			}
			if row := rows[p.seen.Cert.Fingerprint]; row != nil {
				row.NotReloaded = append(row.NotReloaded, p.where)
				if row.Renewed == "" {
					row.Renewed = h.name + ":" + f.Path + " (ends " + f.Cert.NotAfter.Format("2006-01-02") + ", since " + f.Mod.Format("2006-01-02 15:04") + " UTC)"
				}
				for _, rule := range h.last.Reloads {
					if certfiles.Match(rule.Files, f.Path) {
						row.Reloads = append(row.Reloads, CertReload{Host: h.name, Unit: rule.Unit, Addr: p.seen.Addr, Port: p.seen.Port, Where: p.where, File: f.Path})
					}
				}
			}
		}
	}
	// Cloudflare's edge certificate packs: their soonest end, no
	// fingerprint (the API doesn't give one), a row per pack.
	if srv.cf != nil {
		for _, z := range srv.cf.Snapshot().Zones {
			for _, c := range z.Certs {
				if c.Expires.IsZero() {
					continue
				}
				subject := z.Name
				if len(c.Hosts) > 0 {
					subject = c.Hosts[0]
				}
				add(report.Cert{Subject: subject, Names: c.Hosts, Issuer: "Cloudflare (" + c.Type + ")", NotAfter: c.Expires},
					"cf\x00"+z.Name+"\x00"+strings.Join(c.Hosts, ","), CertInvPlace{Kind: "cloudflare", Where: z.Name + " edge", Href: "/cloudflare"})
			}
		}
	}
	// Akamai CPS's deployed certificates (docs/akamai.md, step 2): one
	// place per enrollment; RSA and ECDSA are rows of their own.
	if srv.ak != nil {
		for _, e := range srv.ak.Snapshot().Enrollments {
			for _, c := range e.Deployed {
				add(c, "", CertInvPlace{Kind: "akamai", Where: akamaiEnrollment(e), Href: "/akamai"})
			}
		}
	}
	mergeUnprinted(rows)
	out := make([]*CertInv, 0, len(rows))
	for _, row := range rows {
		row.Days = int(row.NotAfter.Sub(now).Hours() / 24)
		if row.NotAfter.Before(now) {
			row.Days = -int(now.Sub(row.NotAfter).Hours()/24) - 1
		}
		row.Status, row.Word = certStatus(row.Cert, now, srv.certCritical())
		if fileOnly(row) && row.Status == "warning" && row.NotAfter.Sub(now) >= fileWarn {
			row.Status = "good" // a file no port serves is judged from 7 days only
		}
		if row.Weak = certinfo.Weak(&row.Cert); row.Weak != "" && row.Status == "good" && !fileOnly(row) {
			row.Status = "warning"
		}
		if (len(row.NotReloaded) > 0 || len(row.Wrong) > 0) && row.Status == "good" {
			row.Status = "warning"
		}
		if row.Status == "good" {
			row.Renews = nil // nothing to hurry: cert-manager renews at two thirds
		}
		slices.Sort(row.Kinds)
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b *CertInv) int {
		if c := a.NotAfter.Compare(b.NotAfter); c != 0 {
			return c
		}
		return strings.Compare(a.Subject, b.Subject)
	})
	return out
}

// mergeUnprinted joins a row known without its fingerprint (a
// cert-manager Certificate's status, a Cloudflare pack) to the row of the
// certificate for the same names that ends the same second, when there's
// exactly one.
func mergeUnprinted(rows map[string]*CertInv) {
	byEnd := map[string][]string{}
	for k, r := range rows {
		if r.Fingerprint != "" {
			e := namesKey(r.Cert) + "|" + strconv.FormatInt(r.NotAfter.Unix(), 10)
			byEnd[e] = append(byEnd[e], k)
		}
	}
	for k, r := range rows {
		if r.Fingerprint != "" {
			continue
		}
		m := byEnd[namesKey(r.Cert)+"|"+strconv.FormatInt(r.NotAfter.Unix(), 10)]
		if len(m) != 1 {
			continue
		}
		into := rows[m[0]]
		into.PlaceCount += r.PlaceCount
		for _, p := range r.Places {
			if len(into.Places) < certPlaces {
				into.Places = append(into.Places, p)
			}
		}
		for _, kind := range r.Kinds {
			if !slices.Contains(into.Kinds, kind) {
				into.Kinds = append(into.Kinds, kind)
			}
		}
		into.Renewals = max(into.Renewals, r.Renewals)
		delete(rows, k)
	}
}

// hostPort is a port's certificate, for the reload check.
type hostPort struct {
	host, where string
	seen        report.CertSeen
}

// namesKey is what a certificate is for, to match a renewed one: its
// names, or its subject when it has none.
func namesKey(c report.Cert) string {
	if len(c.Names) == 0 {
		return c.Subject
	}
	return strings.Join(c.Names, ",")
}

// fileWarn is when a certificate found only in files starts to warn: 7
// days, not 21 (one no port serves may be nobody's).
const fileWarn = 7 * 24 * time.Hour

// fileOnly says a certificate was found only in files or a Windows
// certificate store.
func fileOnly(c *CertInv) bool {
	for _, k := range c.Kinds {
		if k != "file" && k != "store" {
			return false
		}
	}
	return len(c.Kinds) > 0
}

// certOfRecord is a `cert` record's certificate, from its facts.
func certOfRecord(r report.Workload) report.Cert {
	str := func(k string) string { s, _ := r.Facts[k].(string); return s }
	c := report.Cert{Fingerprint: str("fingerprint"), Subject: r.Name, Issuer: str("issued_by"), Key: str("key"), Sig: str("signature"), NotBefore: r.Since}
	if n := str("names"); n != "" {
		c.Names = strings.Split(n, ",")
	}
	if t, err := time.Parse(time.RFC3339, str("ends")); err == nil {
		c.NotAfter = t
	}
	c.CA, _ = r.Facts["ca"].(bool)
	return c
}

// handleCertificates is the inventory: ?q=<subject, name, issuer or
// place>&within=<days>&kind=<host|file|vault|check|cloudflare|cluster>&status=<problems>
// &offset=&limit= (default 200, at most 1000), soonest end first.
func (srv *Server) handleCertificates(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q := r.URL.Query()
	find := strings.ToLower(strings.TrimSpace(q.Get("q")))
	within, _ := strconv.Atoi(q.Get("within"))
	kind, status := q.Get("kind"), q.Get("status")
	all := srv.certificates(now)
	counts := map[string]int{"all": len(all)}
	kinds := map[string]int{}
	var rows []*CertInv
	for _, c := range all {
		counts[c.Status]++
		if c.Weak != "" {
			counts["weak"]++
		}
		for _, k := range c.Kinds {
			kinds[k]++
		}
		switch {
		case within > 0 && c.Days >= within,
			kind != "" && !slices.Contains(c.Kinds, kind),
			status == "problems" && c.Status == "good",
			find != "" && !certMatches(c, find):
			continue
		}
		rows = append(rows, c)
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 1000)
	page := window(rows, offset, limit)
	srv.lastActions(page, now)
	writeJSON(w, http.StatusOK, map[string]any{"certificates": page, "total": len(rows), "offset": offset, "limit": limit,
		"counts": counts, "kinds": kinds, "can_act": srv.db != nil})
}

func certMatches(c *CertInv, find string) bool {
	if strings.Contains(strings.ToLower(c.Subject), find) || strings.Contains(strings.ToLower(c.Issuer), find) || strings.HasPrefix(c.Fingerprint, find) {
		return true
	}
	for _, n := range c.Names {
		if strings.Contains(strings.ToLower(n), find) {
			return true
		}
	}
	for _, p := range c.Places {
		if strings.Contains(strings.ToLower(p.Where), find) {
			return true
		}
	}
	return false
}

// certConditions are the inventory's own alerts (decision 3): one per
// certificate found on hosts' ports or in their files, naming its places.
// One that a Vault, a site check or Cloudflare also has alerts there
// instead (decision 5), unless a port still serves it after it was
// renewed (decision 4): only the inventory sees that.
func (srv *Server) certConditions(now time.Time) []Condition {
	var conds []Condition
	for _, c := range srv.certificates(now) {
		own := !slices.ContainsFunc(c.Kinds, func(k string) bool { return k != "host" && k != "file" })
		if c.Status == "good" || !own && len(c.NotReloaded) == 0 && !wrongOnHosts(c) {
			continue
		}
		hosts := map[string]bool{}
		var served, files []string
		for _, p := range c.Places {
			switch p.Kind {
			case "host":
				hosts[p.Host] = true
				served = append(served, p.Where)
			case "file":
				hosts[p.Host] = true
				files = append(files, p.Where)
			}
		}
		var what string
		if own {
			what = lowerFirst(c.Word)
			if c.Weak != "" && c.Days >= 21 && !fileOnly(c) {
				what = "is weak: " + c.Weak
			} else if c.Weak != "" {
				what += "; weak: " + c.Weak
			}
		}
		if own && c.Days >= 21 && c.Weak == "" {
			what = "" // "ends 2027-…" says nothing: say what's wrong
		}
		also := func(s string) {
			if what == "" {
				what = s
			} else {
				what += "; " + s
			}
		}
		if len(c.NotReloaded) > 0 {
			also("renewed but not reloaded")
		}
		if wrongOnHosts(c) {
			also("served wrong")
		}
		var detail []string
		if len(served) > 0 {
			detail = append(detail, "Served on "+listSome(served, c.PlaceCount-len(c.Places))+".")
		}
		if len(files) > 0 {
			in := "In " + listSome(files, 0)
			if len(served) == 0 {
				in += "; no port serves it"
			}
			detail = append(detail, in+".")
		}
		if len(c.NotReloaded) > 0 {
			detail = append(detail, "A renewed one is in "+c.Renewed+", but "+strings.Join(c.NotReloaded, ", ")+" still serves this one: reload the service.")
		}
		if wrongOnHosts(c) {
			detail = append(detail, "Served wrong: "+listSome(hostWrong(c), 0)+".")
		}
		sev := c.Status
		if !own {
			sev = "warning" // the others alert on its end; this is only the reload
		}
		cond := Condition{Key: "certinv/" + c.Fingerprint, Severity: sev, Ends: c.NotAfter, Title: "Certificate " + c.Subject + " " + what,
			Detail: strings.Join(detail, " "), Href: "/certificates?q=" + c.Fingerprint[:16]}
		if len(hosts) == 1 {
			for h := range hosts {
				cond.Host = h // a maintenance window on the host holds it
			}
		}
		if own && c.Weak == "" && len(c.NotReloaded) == 0 && !wrongOnHosts(c) && c.Issuer != "" && c.NotAfter.After(now) {
			cond.Batch = &Batch{Key: "certends/issuer/" + c.Issuer, Title: "{n} certificates from " + c.Issuer + " end soon",
				Item: c.Subject + " (" + certLeft(c.NotAfter.Sub(now)) + ")", Href: "/certificates?q=" + url.QueryEscape(c.Issuer)}
		}
		conds = append(conds, cond)
	}
	return conds
}

// hostWrong is what's wrong with how hosts' ports serve it: a cluster's
// handshakes alert with its records.
func hostWrong(c *CertInv) []string {
	var out []string
	for _, w := range c.Wrong {
		for _, p := range c.Places {
			if p.Kind == "host" && strings.HasPrefix(w, p.Where+": ") {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

func wrongOnHosts(c *CertInv) bool { return len(hostWrong(c)) > 0 }

// listSome is the first three of a list, and how many more.
func listSome(l []string, more int) string {
	if len(l) > 3 {
		l, more = l[:3], more+len(l)-3
	}
	s := strings.Join(l, ", ")
	if more > 0 {
		s += " and " + strconv.Itoa(more) + " more"
	}
	return s
}
