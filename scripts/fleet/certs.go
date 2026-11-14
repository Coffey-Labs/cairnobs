package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The fake fleet's certificates (docs/cert-inventory.md, steps 2 to 5),
// by a host's place in its site (netmap.go):
//
//	0      the gateway: RHEL's own localhost.crt in a file, served nowhere
//	1-2    load balancers: the fleet's *.example.com on :443 (haproxy),
//	       from /etc/haproxy/certs; site 3's second one still serves the
//	       last one though its file was renewed, and site 5's sends it
//	       without its intermediate
//	30-39  PostgreSQL: each its own certificate from the internal CA
//	       (STARTTLS), and in /etc/pki/tls/certs; a few end soon, one
//	       has a 1024-bit key
//
// Clusters have the router's (or ingress controller's) wildcard on every
// Route or Ingress, and cert-manager Certificates for ten of their apps.

const certResend = 30 * time.Minute

var certEpoch = time.Now().UTC().Truncate(time.Hour)

func fakeFingerprint(seed string) string {
	sum := sha256.Sum256([]byte("fleet-cert:" + seed))
	return hex.EncodeToString(sum[:])
}

// fakeCert is a certificate that began begun ago and lasts life.
func fakeCert(seed, subject string, names []string, issuer, key string, begun, life time.Duration) report.Cert {
	nb := certEpoch.Add(-begun)
	return report.Cert{Fingerprint: fakeFingerprint(seed), Subject: subject, Names: names, Issuer: issuer,
		NotBefore: nb, NotAfter: nb.Add(life), Key: key, Sig: "ECDSA-SHA384"}
}

var (
	// The fleet's wildcard, from a public CA: 90 days, 40 in.
	wildcard = fakeCert("wildcard-2", "*.example.com", []string{"*.example.com", "example.com"}, "E7", "ECDSA P-256", 40*24*time.Hour, 90*24*time.Hour)
	// The one before it, still served where nobody reloaded.
	wildcardOld = fakeCert("wildcard-1", "*.example.com", []string{"*.example.com", "example.com"}, "E7", "ECDSA P-256", 80*24*time.Hour, 90*24*time.Hour)
)

// hostCerts are what host i's ports serve and its files hold; nil for
// none.
func hostCerts(i int, name string) (*report.CertScan, *report.CertFiles) {
	n := fleetIndex(i, name)
	site, pos := n/siteSize, n%siteSize
	var seen []report.CertSeen
	var files []report.CertFile
	file := func(path string, c report.Cert, mod time.Time) {
		files = append(files, report.CertFile{Path: path, Mod: mod, Cert: c})
	}
	switch {
	case pos == 0:
		c := fakeCert("localhost-"+name, name, []string{name}, name, "RSA 2048", 400*24*time.Hour, 365*24*time.Hour*10)
		c.CA = true
		file("/etc/pki/tls/certs/localhost.crt", c, c.NotBefore)
	case pos <= 2:
		c := wildcard
		s := report.CertSeen{Addr: "127.0.0.1", Port: 443, Owner: "unit:haproxy.service", How: "tls", Cert: c, Chain: 1}
		switch {
		case site == 3 && pos == 2:
			s.Cert = wildcardOld // renewed on disk, haproxy never reloaded
		case site == 5 && pos == 1:
			s.Chain, s.Wrong = 0, []string{"sent without its intermediate certificate"}
		}
		seen = append(seen, s)
		file("/etc/haproxy/certs/example.com.pem", c, wildcard.NotBefore.Add(time.Hour))
	case pos >= 30 && pos < 40:
		begun := time.Duration(30+(n*37)%300) * 24 * time.Hour
		switch {
		case n%97 == 30: // ends in a few days
			begun = 360 * 24 * time.Hour
		case n%131 == 31: // ended
			begun = 370 * 24 * time.Hour
		}
		key := "ECDSA P-256"
		if n%211 == 32 {
			key = "RSA 1024"
		}
		cn := name + ".db.internal"
		c := fakeCert("pg-"+name, cn, []string{cn}, "Fleet Internal CA", key, begun, 365*24*time.Hour)
		seen = append(seen, report.CertSeen{Addr: "127.0.0.1", Port: 5432, Owner: "unit:postgresql.service", How: "postgres", Cert: c, Chain: 1})
		file("/etc/pki/tls/certs/postgresql.crt", c, c.NotBefore)
	}
	var scan *report.CertScan
	if seen != nil {
		scan = &report.CertScan{List: seen}
	}
	var held *report.CertFiles
	if files != nil {
		held = &report.CertFiles{List: files}
	}
	return scan, held
}

// stampCerts sends a host's lists as the agent does: at first and every
// half hour, each port's as tried now.
func (h *host) stampCerts(r *report.Report, now time.Time) {
	if (h.certs == nil && h.files == nil) || now.Sub(h.certsSent) < certResend {
		return
	}
	h.certsSent = now
	if h.certs != nil {
		c := *h.certs
		c.At, c.List = now.UTC(), append([]report.CertSeen{}, h.certs.List...)
		for i := range c.List {
			c.List[i].At = now.UTC()
		}
		r.Certs = &c
	}
	if h.files != nil {
		f := *h.files
		f.At = now.UTC()
		r.CertFiles = &f
	}
}

// clusterCerts are a cluster's cert records: the router's wildcard on its
// Routes (or the ingress controller's on its Ingresses) and cert-manager
// Certificates for its first ten apps.
func clusterCerts(name string, ocp bool, routes int, put func(report.Workload)) {
	k := clusterIndex(name)
	domain := "apps." + name + ".example.com"
	issuer := "ingress-operator@1731000000"
	if !ocp {
		issuer = "R11"
	}
	w := fakeCert("router-"+name, "*."+domain, []string{"*." + domain}, issuer, "RSA 2048", 200*24*time.Hour, 730*24*time.Hour)
	kind := "route"
	if !ocp {
		kind = "ingress"
	}
	var places []string
	for n := range routes {
		app := appNames[n]
		places = append(places, fmt.Sprintf("ns-%02d/%s/%s %s.%s", n, kind, app, app, domain))
	}
	shown := places[:min(5, len(places))]
	on := strings.Join(shown, ", ")
	if len(places) > len(shown) {
		on += fmt.Sprintf(" and %d more", len(places)-len(shown))
	}
	if len(places) > 0 {
		put(report.Workload{Kind: report.KindCert, ID: "served/" + w.Fingerprint[:16], Name: w.Subject, State: report.StateOK,
			Word: "Ends in " + fmt.Sprint(int(w.NotAfter.Sub(time.Now()).Hours()/24)) + " days (" + w.NotAfter.Format("2006-01-02") + ")", Since: w.NotBefore,
			Facts: map[string]any{"ends": w.NotAfter.Format(time.RFC3339), "names": strings.Join(w.Names, ","), "issued_by": w.Issuer,
				"fingerprint": w.Fingerprint, "key": w.Key, "signature": w.Sig, "served_on": on, "places": len(places)}})
	}
	for n := range min(10, routes) {
		app := appNames[n]
		host := app + ".example.com"
		begun := time.Duration(10+(n*7+k*3)%60) * 24 * time.Hour
		if n == 4 && k == 0 {
			begun = 86 * 24 * time.Hour // ends in four days: the renewal is failing
		}
		c := fakeCert("cm-"+name+"-"+app, host, []string{host}, "", "ECDSA P-256", begun, 90*24*time.Hour)
		r := report.Workload{Kind: report.KindCert, ID: fmt.Sprintf("cm-%s-%02d", name, n), Name: host, Namespace: fmt.Sprintf("ns-%02d", n), Since: c.NotBefore,
			Facts: map[string]any{"certificate": app + "-tls", "issued_by": "ClusterIssuer letsencrypt", "names": host,
				"ends": c.NotAfter.Format(time.RFC3339), "renews": c.NotAfter.Add(-30 * 24 * time.Hour).Format(time.RFC3339)}}
		left := time.Until(c.NotAfter)
		r.State, r.Word = report.StateOK, fmt.Sprintf("Ends in %d days (%s)", int(left.Hours()/24), c.NotAfter.Format("2006-01-02"))
		switch {
		case n == 3:
			r.State, r.Word, r.Since = report.StatePending, "Not issued yet: Issuing certificate as Secret does not exist", time.Now().Add(-2*time.Hour)
			delete(r.Facts, "ends")
		case left < 7*24*time.Hour:
			r.State, r.Word = report.StateDown, "Not ready: The certificate request has failed to complete and will be retried; "+strings.ToLower(r.Word[:1])+r.Word[1:]
		case left < 21*24*time.Hour:
			r.State = report.StateDegraded
		}
		put(r)
	}
}
