package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/certfiles"
	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/vault"
)

func testCert(t *testing.T, cn string, key any, pub any, left time.Duration) *report.Cert {
	t.Helper()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-300 * 24 * time.Hour), NotAfter: time.Now().Add(left)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	return certinfo.Of(x)
}

// The inventory gathers Vault's records and site checks' handshakes, one
// row per certificate whatever the number of places it was seen.
func TestCertificates(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	shared := testCert(t, "shop.example.org", ec, &ec.PublicKey, 10*24*time.Hour)
	rk, _ := rsa.GenerateKey(rand.Reader, 1024)
	weak := testCert(t, "old.example.org", rk, &rk.PublicKey, 200*24*time.Hour)

	// Vault issued the shop's certificate; two hosts' checks are served it.
	srv.cfg.Vault = []vault.Config{{Name: "vp"}}
	rec := report.Workload{Kind: report.KindCert, ID: "cert/abc", Name: "shop.example.org", Namespace: "pki_int", State: report.StateDegraded,
		Word: "Ends in 10 days", Since: shared.NotBefore, Facts: map[string]any{"fingerprint": shared.Fingerprint, "names": "shop.example.org",
			"ends": shared.NotAfter.Format(time.RFC3339), "issued_by": "Int CA", "key": shared.Key, "signature": shared.Sig, "certificates": 3}}
	j, _ := json.Marshal(rec)
	_ = json.Unmarshal(j, &rec) // as stored: numbers come back float64
	_ = srv.storeWorkloads(serverSource+":vp", report.Workloads{Full: report.FullBegin}, now)
	_ = srv.storeWorkloads(serverSource+":vp", report.Workloads{Full: report.FullEnd, Records: []report.Workload{rec}}, now)
	srv.store.Put("host2", results(now, report.CheckResult{ID: "site", OK: true, Cert: shared}), now)
	srv.store.Put("host3", results(now, report.CheckResult{ID: "site", OK: true, Cert: shared}, report.CheckResult{ID: "mx", OK: true, Cert: weak}), now)

	// host1 serves the shop's certificate too, and one of its own.
	mine := testCert(t, "admin.internal", ec, &ec.PublicKey, 5*24*time.Hour)
	r := results(now)
	r.Certs = &report.CertScan{At: now, List: []report.CertSeen{
		{Addr: "127.0.0.1", Port: 443, Owner: "nginx", How: "tls", Cert: *shared, At: now},
		{Addr: "127.0.0.1", Port: 8443, Owner: "admin", How: "tls", Cert: *mine, At: now},
		{Addr: "127.0.0.1", Port: 25, Owner: "postfix", How: "smtp", Cert: *mine, At: now},
	}}
	srv.store.Put("host1", r, now)
	srv.store.Put("host1", results(now.Add(time.Second)), now.Add(time.Second)) // kept between scans

	conds := srv.certConditions(now)
	if len(conds) != 1 || conds[0].Key != "certinv/"+mine.Fingerprint || conds[0].Severity != "critical" || conds[0].Host != "host1" ||
		conds[0].Detail != "Served on host1:8443 · admin, host1:25 (smtp STARTTLS) · postfix." {
		t.Errorf("conditions: %+v", conds)
	}
	rows := srv.certificates(now)
	if len(rows) != 3 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	rows = slices.DeleteFunc(rows, func(c *CertInv) bool { return c.Subject == "admin.internal" })
	if len(rows) != 2 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	s := rows[0]
	if s.Subject != "shop.example.org" || s.PlaceCount != 4 || len(s.Kinds) != 3 || s.Kinds[0] != "check" || s.Kinds[1] != "host" || s.Kinds[2] != "vault" ||
		s.Status != "warning" || s.Days != 9 || s.Renewals != 3 || s.Key != "ECDSA P-256" {
		t.Errorf("the shop's: %+v", s)
	}
	if w := rows[1]; w.Weak != "RSA 1024 key" || w.Status != "warning" || w.PlaceCount != 1 || w.Places[0].Kind != "check" || w.Places[0].Href != "/checks" {
		t.Errorf("the weak one: %+v", w)
	}

	cookie := post(srv.Handler(), "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	var got struct {
		Certificates []CertInv
		Total        int
		Counts       map[string]int
	}
	_ = json.Unmarshal(get(srv.Handler(), "/api/v1/certificates?within=21&kind=vault", cookie).Body.Bytes(), &got)
	if got.Total != 1 || got.Certificates[0].Subject != "shop.example.org" || got.Counts["all"] != 3 || got.Counts["weak"] != 1 {
		t.Errorf("API: %+v", got)
	}
	_ = json.Unmarshal(get(srv.Handler(), "/api/v1/certificates?q=host3", cookie).Body.Bytes(), &got)
	if got.Total != 2 {
		t.Errorf("found by place: %+v", got)
	}
}

// Step 3: files' certificates join the page; one no port serves is judged
// at 7 days only; a port still serving the older certificate an hour after
// its file was renewed warns (decision 4), even one a check alerts on.
func TestCertFiles(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	old := testCert(t, "admin.internal", ec, &ec.PublicKey, 5*24*time.Hour)
	renewed := testCert(t, "admin.internal", ec, &ec.PublicKey, 85*24*time.Hour)
	spare := testCert(t, "spare.internal", ec, &ec.PublicKey, 15*24*time.Hour)
	ending := testCert(t, "ending.internal", ec, &ec.PublicKey, 3*24*time.Hour)
	shop := testCert(t, "shop.example.org", ec, &ec.PublicKey, 40*24*time.Hour)
	shopNew := testCert(t, "shop.example.org", ec, &ec.PublicKey, 90*24*time.Hour)

	srv.store.Put("host2", results(now, report.CheckResult{ID: "site", OK: true, Cert: shop}), now)
	r := results(now)
	r.Certs = &report.CertScan{At: now, List: []report.CertSeen{
		{Addr: "127.0.0.1", Port: 8443, Owner: "admin", How: "tls", Cert: *old, At: now},
		{Addr: "127.0.0.1", Port: 443, Owner: "nginx", How: "tls", Cert: *shop, At: now},
	}}
	r.CertFiles = &report.CertFiles{At: now, List: []report.CertFile{
		{Path: "/etc/letsencrypt/live/admin/cert.pem", Mod: now.Add(-3 * time.Hour), Cert: *renewed},
		{Path: "/etc/letsencrypt/live/shop/cert.pem", Mod: now.Add(-3 * time.Hour), Cert: *shopNew},
		{Path: "/etc/pki/tls/certs/spare.crt", Mod: now.Add(-48 * time.Hour), Cert: *spare},
		{Path: "/etc/pki/tls/certs/ending.crt", Mod: now.Add(-48 * time.Hour), Cert: *ending},
	}}
	srv.store.Put("host1", r, now)
	srv.store.Put("host1", results(now.Add(time.Second)), now.Add(time.Second)) // kept between reports

	rows := map[string]*CertInv{}
	for _, c := range srv.certificates(now) {
		rows[c.Fingerprint] = c
	}
	if c := rows[old.Fingerprint]; c == nil || c.Status != "critical" || len(c.NotReloaded) != 1 || c.NotReloaded[0] != "host1:8443 · admin" ||
		!strings.HasPrefix(c.Renewed, "host1:/etc/letsencrypt/live/admin/cert.pem (ends ") {
		t.Errorf("the admin's: %+v", c)
	}
	if c := rows[renewed.Fingerprint]; c == nil || c.Status != "good" || len(c.Kinds) != 1 || c.Kinds[0] != "file" || c.Places[0].Where != "host1:/etc/letsencrypt/live/admin/cert.pem" {
		t.Errorf("the renewed one: %+v", c)
	}
	if c := rows[spare.Fingerprint]; c == nil || c.Status != "good" {
		t.Errorf("a spare file isn't judged at 21 days: %+v", c)
	}

	conds := map[string]Condition{}
	for _, c := range srv.certConditions(now) {
		conds[c.Key] = c
	}
	if len(conds) != 3 {
		t.Errorf("%d conditions: %+v", len(conds), conds)
	}
	if c := conds["certinv/"+old.Fingerprint]; c.Severity != "critical" || c.Host != "host1" || !strings.HasSuffix(c.Title, "; renewed but not reloaded") ||
		!strings.Contains(c.Detail, "but host1:8443 · admin still serves this one: reload the service.") {
		t.Errorf("the admin's: %+v", c)
	}
	if c := conds["certinv/"+shop.Fingerprint]; c.Severity != "warning" || c.Title != "Certificate shop.example.org renewed but not reloaded" {
		t.Errorf("the shop's, which a check alerts on: %+v", c)
	}
	if c := conds["certinv/"+ending.Fingerprint]; c.Severity != "critical" || c.Detail != "In host1:/etc/pki/tls/certs/ending.crt; no port serves it." {
		t.Errorf("a file ending in 3 days: %+v", c)
	}

	// Just renewed, or not tried since: no warning yet.
	r.CertFiles.List[0].Mod = now.Add(-30 * time.Minute)
	srv.store.Put("host1", r, now)
	if c := srv.certificates(now); slices.ContainsFunc(c, func(c *CertInv) bool { return c.Fingerprint == old.Fingerprint && len(c.NotReloaded) > 0 }) {
		t.Error("warned within the hour")
	}
}

// A Windows host's certificate store is listed as a store, not a file,
// and judged as a file is: at 7 days, not 21.
func TestCertStore(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	iis := testCert(t, "intranet.corp.example", ec, &ec.PublicKey, 15*24*time.Hour)
	r := results(now)
	r.CertFiles = &report.CertFiles{At: now, List: []report.CertFile{
		{Path: certfiles.StorePath("My", []byte("der")), Mod: iis.NotBefore, Cert: *iis},
	}}
	srv.store.Put("win1", r, now)
	var row *CertInv
	for _, c := range srv.certificates(now) {
		if c.Fingerprint == iis.Fingerprint {
			row = c
		}
	}
	if row == nil || row.Status != "good" || len(row.Kinds) != 1 || row.Kinds[0] != "store" ||
		!strings.HasPrefix(row.Places[0].Where, `win1:Cert:\LocalMachine\My\`) || len(row.Places[0].Where) != len(`win1:Cert:\LocalMachine\My\`)+40 {
		t.Errorf("the store's: %+v", row)
	}
}

// Step 4: a cert-manager Certificate known only by its status joins the
// row of the certificate a site check was served, by its names and end;
// its alert stays the cluster's.
func TestCertMerge(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	shop := testCert(t, "shop.example.org", ec, &ec.PublicKey, 5*24*time.Hour)
	rec := report.Workload{Kind: report.KindCert, ID: "c1", Name: "shop.example.org", Namespace: "shop", State: report.StateDown,
		Word: "Ends in 5 days", Since: shop.NotBefore, Facts: map[string]any{"names": "shop.example.org", "ends": shop.NotAfter.Format(time.RFC3339),
			"certificate": "shop-tls", "issued_by": "ClusterIssuer letsencrypt", "served_on": "shop/ingress/shop shop.example.org"}}
	srv.store.Put("host2", results(now, report.CheckResult{ID: "site", OK: true, Cert: shop}), now)
	// What host2's agent reads beside it stands in for a cluster agent.
	_ = srv.storeWorkloads("host2:cluster-a", report.Workloads{Full: report.FullBegin}, now)
	_ = srv.storeWorkloads("host2:cluster-a", report.Workloads{Full: report.FullEnd, Records: []report.Workload{rec}}, now)

	rows := srv.certificates(now)
	if len(rows) != 1 || rows[0].PlaceCount != 2 || strings.Join(rows[0].Kinds, ",") != "check,cluster" || rows[0].Fingerprint != shop.Fingerprint {
		t.Fatalf("%d rows: %+v", len(rows), *rows[0])
	}
	var where string
	for _, p := range rows[0].Places {
		if p.Kind == "cluster" {
			where = p.Where
		}
	}
	if !strings.HasSuffix(where, " · shop · Certificate shop-tls · served on shop/ingress/shop shop.example.org") {
		t.Errorf("place: %q", where)
	}
	if c := srv.certConditions(now); len(c) != 0 {
		t.Errorf("the inventory alerts too: %+v", c)
	}
}

// Step 5: a port serving a certificate wrong warns, on the certificate's
// own alert, or alone for one a check alerts on.
func TestCertWrong(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	admin := testCert(t, "admin.internal", ec, &ec.PublicKey, 200*24*time.Hour)
	shop := testCert(t, "shop.example.org", ec, &ec.PublicKey, 200*24*time.Hour)
	srv.store.Put("host2", results(now, report.CheckResult{ID: "site", OK: true, Cert: shop}), now)
	r := results(now)
	r.Certs = &report.CertScan{At: now, List: []report.CertSeen{
		{Addr: "127.0.0.1", Port: 8443, Owner: "admin", How: "tls", Cert: *admin, At: now, Wrong: []string{"self-signed where a CA-signed certificate was"}},
		{Addr: "127.0.0.1", Port: 443, Owner: "nginx", How: "tls", Cert: *shop, At: now, Wrong: []string{"sent without its intermediate certificate"}},
	}}
	srv.store.Put("host1", r, now)

	conds := map[string]Condition{}
	for _, c := range srv.certConditions(now) {
		conds[c.Key] = c
	}
	if c := conds["certinv/"+admin.Fingerprint]; c.Severity != "warning" || c.Title != "Certificate admin.internal served wrong" ||
		c.Detail != "Served on host1:8443 · admin. Served wrong: host1:8443 · admin: self-signed where a CA-signed certificate was." {
		t.Errorf("its own: %+v", c)
	}
	if c := conds["certinv/"+shop.Fingerprint]; c.Severity != "warning" || c.Title != "Certificate shop.example.org served wrong" {
		t.Errorf("one a check has: %+v", c)
	}
	for _, row := range srv.certificates(now) {
		if row.Status != "warning" || len(row.Wrong) != 1 {
			t.Errorf("row: %+v", *row)
		}
	}
}
