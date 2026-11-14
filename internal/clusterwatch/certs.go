package clusterwatch

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/tlsscan"
)

// The certificate inventory in a cluster (docs/cert-inventory.md, step 4):
// cert-manager's Certificates, from their status (no Secret is read), and
// what each TLS Ingress and Route host serves, from a handshake as an
// outsider would make (or a Route's own certificate, inline). Each is a
// `cert` record, judged here as Vault's are: one record per certificate,
// listing where it's served.

// cmCertificate is cert-manager.io/v1 Certificate.
type cmCertificate struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		CommonName  string   `json:"commonName"`
		DNSNames    []string `json:"dnsNames"`
		IPAddresses []string `json:"ipAddresses"`
		URIs        []string `json:"uris"`
		SecretName  string   `json:"secretName"`
		IssuerRef   struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"issuerRef"`
	} `json:"spec"`
	Status struct {
		NotBefore   *time.Time  `json:"notBefore"`
		NotAfter    *time.Time  `json:"notAfter"`
		RenewalTime *time.Time  `json:"renewalTime"`
		Conditions  []condition `json:"conditions"`
	} `json:"status"`
}

// tlsSeenRes is the store's pseudo-resource of handshakes: host → the
// report.CertSeen it served, kept by the scanner's goroutine.
const tlsSeenRes = "tlsseen"

var nowFunc = time.Now

const (
	certWarn     = 21 * 24 * time.Hour
	certCritical = 7 * 24 * time.Hour
	servedShown  = 5 // places named in served_on; places has them all
)

// servedAt is one Ingress or Route host with TLS.
type servedAt struct {
	ns, secret string
	where      string // "shop/ingress/web shop.example.org"
	host       string
	inline     *report.Cert // a Route's own certificate
	wrong      []string     // what's wrong with it (step 5): its name
}

// tlsHosts are the hosts of TLS Ingresses and Routes, which the scanner
// tries, and where each is.
func tlsHosts(v view) []servedAt {
	var out []servedAt
	for _, i := range each[ingress](v, "ingresses") {
		for _, t := range i.Spec.TLS {
			hosts := t.Hosts
			if len(hosts) == 0 { // the TLS entry covers the rules' hosts
				for _, r := range i.Spec.Rules {
					hosts = append(hosts, r.Host)
				}
			}
			for _, h := range hosts {
				if h != "" {
					out = append(out, servedAt{ns: i.Metadata.Namespace, secret: t.SecretName, host: h,
						where: i.Metadata.Namespace + "/ingress/" + i.Metadata.Name + " " + h})
				}
			}
		}
	}
	for _, rt := range each[route](v, "routes") {
		if rt.Spec.TLS == nil || rt.Spec.Host == "" {
			continue
		}
		s := servedAt{ns: rt.Metadata.Namespace, host: rt.Spec.Host, where: rt.Metadata.Namespace + "/route/" + rt.Metadata.Name + " " + rt.Spec.Host}
		if b, _ := pem.Decode([]byte(rt.Spec.TLS.Certificate)); b != nil && b.Type == "CERTIFICATE" {
			if x, err := x509.ParseCertificate(b.Bytes); err == nil {
				s.inline = certinfo.Of(x)
				if x.VerifyHostname(s.host) != nil {
					s.wrong = []string{"doesn't cover " + s.host}
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// scanNames are the hosts the scanner tries: those without an inline
// certificate, each once.
func scanNames(v view) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range tlsHosts(v) {
		if s.inline == nil && !seen[s.host] {
			seen[s.host] = true
			out = append(out, s.host)
		}
	}
	slices.Sort(out)
	return out
}

// certRecords are cert-manager's Certificates and the certificates TLS
// hosts serve. A served certificate whose Secret a Certificate fills
// joins that Certificate's record; the others are a record each, by
// fingerprint.
func (b *builder) certRecords(v view, now time.Time) []report.Workload {
	seen := map[string]report.CertSeen{}
	for h, o := range v[tlsSeenRes] {
		if c, ok := o.(report.CertSeen); ok {
			seen[h] = c
		}
	}
	type group struct {
		cert   report.Cert
		places []string
		ns     map[string]bool
		wrong  []string // "<place>: <what>"
	}
	managed := map[string]*group{} // ns/secret → what's served from it
	served := map[string]*group{}  // fingerprint → where
	add := func(m map[string]*group, k string, c report.Cert, s servedAt) {
		g := m[k]
		if g == nil {
			g = &group{cert: c, ns: map[string]bool{}}
			m[k] = g
		}
		g.places = append(g.places, s.where)
		g.ns[s.ns] = true
		for _, w := range s.wrong {
			g.wrong = append(g.wrong, s.where+": "+w)
		}
	}
	cms := each[cmCertificate](v, "certificates")
	bySecret := map[string]bool{}
	for _, c := range cms {
		bySecret[c.Metadata.Namespace+"/"+c.Spec.SecretName] = true
	}
	for _, s := range tlsHosts(v) {
		c := s.inline
		if c == nil {
			if sv, ok := seen[s.host]; ok {
				c, s.wrong = &sv.Cert, sv.Wrong
			}
		}
		if c == nil {
			continue
		}
		if k := s.ns + "/" + s.secret; s.secret != "" && bySecret[k] {
			add(managed, k, *c, s)
			continue
		}
		add(served, c.Fingerprint, *c, s)
	}

	var out []report.Workload
	for _, c := range cms {
		r := b.rec(report.KindCert, c.Metadata.Name, c.Metadata)
		names := append(append(append([]string{}, c.Spec.DNSNames...), c.Spec.IPAddresses...), c.Spec.URIs...)
		slices.Sort(names)
		if c.Spec.CommonName != "" {
			r.Name = c.Spec.CommonName
		} else if len(names) > 0 {
			r.Name = names[0]
		}
		// Its Secret's name stays in the cluster: it only matches what's
		// served from it.
		r.Facts = map[string]any{"certificate": c.Metadata.Name, "issued_by": strings.TrimSpace(c.Spec.IssuerRef.Kind + " " + c.Spec.IssuerRef.Name)}
		if len(names) > 0 {
			r.Facts["names"] = strings.Join(names, ",")
		}
		if t := c.Status.RenewalTime; t != nil {
			r.Facts["renews"] = t.UTC().Format(time.RFC3339)
		}
		g := managed[c.Metadata.Namespace+"/"+c.Spec.SecretName]
		if g != nil {
			servedFacts(r.Facts, g.places)
			// What's served is this Certificate's when it ends when the
			// status says: then its fingerprint is known.
			if c.Status.NotAfter != nil && g.cert.NotAfter.Equal(c.Status.NotAfter.UTC().Truncate(time.Second)) {
				certFacts(r.Facts, g.cert)
			}
		}
		if is := findCond(c.Status.Conditions, "Issuing"); is != nil && is.Status == "True" {
			r.Facts["issuing"] = true // being renewed now: nothing to ask
		}
		ready := findCond(c.Status.Conditions, "Ready")
		switch {
		case c.Status.NotAfter == nil:
			r.State, r.Word, r.Since = report.StatePending, "Not issued yet", c.Metadata.CreationTimestamp
			if ready != nil && ready.Status == "False" && ready.Message != "" {
				r.Word += ": " + short(ready.Message, 160)
			}
		default:
			if c.Status.NotBefore != nil {
				r.Since = c.Status.NotBefore.UTC()
			}
			r.Facts["ends"] = c.Status.NotAfter.UTC().Format(time.RFC3339)
			var life time.Duration
			if c.Status.NotBefore != nil {
				life = c.Status.NotAfter.Sub(*c.Status.NotBefore)
			}
			r.State, r.Word = certState(*c.Status.NotAfter, life, now)
			if ready != nil && ready.Status == "False" {
				if r.State == report.StateOK {
					r.State = report.StateDegraded
				}
				r.Word = "Not ready: " + short(ready.Message, 160) + "; " + lowerFirst(r.Word)
			}
		}
		if g != nil {
			servedWrong(&r, g.wrong)
		}
		out = append(out, r)
	}
	for fp, g := range served {
		ns := ""
		if len(g.ns) == 1 {
			for n := range g.ns {
				ns = n
			}
		}
		r := report.Workload{Kind: report.KindCert, ID: "served/" + fp[:min(16, len(fp))], Name: g.cert.Subject, Namespace: ns, Since: g.cert.NotBefore,
			Facts: map[string]any{"ends": g.cert.NotAfter.UTC().Format(time.RFC3339)}}
		if len(g.cert.Names) > 0 {
			r.Facts["names"] = strings.Join(g.cert.Names, ",")
		}
		if g.cert.Issuer != "" {
			r.Facts["issued_by"] = g.cert.Issuer
		}
		if g.cert.CA {
			r.Facts["ca"] = true
		}
		certFacts(r.Facts, g.cert)
		servedFacts(r.Facts, g.places)
		r.State, r.Word = certState(g.cert.NotAfter, g.cert.NotAfter.Sub(g.cert.NotBefore), now)
		servedWrong(&r, g.wrong)
		out = append(out, r)
	}
	return out
}

// servedWrong says what's wrong with how it's served: a warning at least.
func servedWrong(r *report.Workload, wrong []string) {
	if len(wrong) == 0 {
		return
	}
	slices.Sort(wrong)
	r.Facts["wrong"] = strings.Join(wrong, "; ")
	if r.State == report.StateOK {
		r.State = report.StateDegraded
	}
	r.Word += "; served wrong: " + wrong[0]
	if len(wrong) > 1 {
		r.Word += fmt.Sprintf(" and %d more", len(wrong)-1)
	}
}

func certFacts(f map[string]any, c report.Cert) {
	for k, v := range map[string]string{"fingerprint": c.Fingerprint, "key": c.Key, "signature": c.Sig} {
		if v != "" {
			f[k] = v
		}
	}
}

func servedFacts(f map[string]any, places []string) {
	slices.Sort(places)
	shown := places[:min(len(places), servedShown)]
	s := strings.Join(shown, ", ")
	if len(places) > len(shown) {
		s += fmt.Sprintf(" and %d more", len(places)-len(shown))
	}
	f["served_on"] = s
	f["places"] = len(places)
}

// certState judges by 21 and 7 days, or a third and a tenth of a short
// life, as obs-beacond judges Vault's leaves.
func certState(end time.Time, life time.Duration, now time.Time) (string, string) {
	warn, crit := certWarn, certCritical
	if life > 0 {
		warn, crit = min(warn, life/3), min(crit, life/10)
	}
	left, date := end.Sub(now), end.UTC().Format("2006-01-02")
	switch {
	case left <= 0:
		return report.StateDown, "Ended " + date
	case left < crit:
		return report.StateDown, "Ends in " + daysLeft(left) + " (" + date + ")"
	case left < warn:
		return report.StateDegraded, "Ends in " + daysLeft(left) + " (" + date + ")"
	}
	return report.StateOK, "Ends in " + daysLeft(left) + " (" + date + ")"
}

func daysLeft(d time.Duration) string {
	if d < 48*time.Hour {
		return strconv.Itoa(int(d.Hours())) + " hours"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// scanHosts tries each TLS Ingress and Route host once a day (tlsscan's
// pace), keeping what each served in the store for certRecords.
func scanHosts(ctx context.Context, name string, s *store) {
	sc := tlsscan.New(name)
	t := time.NewTicker(tlsscan.RoundEvery)
	defer t.Stop()
	var listed time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if now.Sub(listed) >= time.Minute { // the hosts change slowly
				sc.UpdateNames(scanNames(s.snapshot()), now)
				listed = now
			}
			sc.Round(ctx, now)
			got := sc.Named()
			s.mu.Lock()
			m := make(map[string]any, len(got))
			changed := len(got) != len(s.objs[tlsSeenRes])
			for h, c := range got {
				m[h] = c
				if o, ok := s.objs[tlsSeenRes][h].(report.CertSeen); !ok || o.Cert.Fingerprint != c.Cert.Fingerprint {
					changed = true
				}
			}
			s.objs[tlsSeenRes] = m
			s.mu.Unlock()
			if changed {
				s.changed()
			}
		}
	}
}
