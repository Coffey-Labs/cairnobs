package clusterwatch

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

func mkCert(t *testing.T, cn string, names []string, end time.Time) (*report.Cert, string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: names,
		NotBefore: end.Add(-90 * 24 * time.Hour), NotAfter: end}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	kb, _ := x509.MarshalECPrivateKey(k)
	return certinfo.Of(x), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

// Step 4: cert-manager's Certificates, from their status; what TLS
// Ingress and Route hosts serve, one record per certificate; a Route's
// inline certificate read without its key.
func TestCertRecords(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	shop, _, _ := mkCert(t, "shop.example.org", []string{"shop.example.org"}, now.Add(10*24*time.Hour))
	wild, _, _ := mkCert(t, "*.apps.example.org", []string{"*.apps.example.org"}, now.Add(300*24*time.Hour))
	inline, inlinePEM, inlineKey := mkCert(t, "pay.example.org", []string{"pay.example.org"}, now.Add(60*24*time.Hour))

	v := view{}
	js := func(o any) string { b, _ := json.Marshal(o); return string(b) }
	v.add(t, "certificates", `{"metadata":{"uid":"c1","name":"shop-cert","namespace":"shop"},"spec":{"dnsNames":["shop.example.org"],"secretName":"shop-tls",
		"issuerRef":{"name":"letsencrypt","kind":"ClusterIssuer"}},"status":{"notBefore":"`+shop.NotBefore.Format(time.RFC3339)+`","notAfter":"`+shop.NotAfter.Format(time.RFC3339)+`",
		"renewalTime":"`+now.Add(-24*time.Hour).Format(time.RFC3339)+`","conditions":[{"type":"Ready","status":"False","message":"Issuing certificate as Secret was previously issued by a different issuer"}]}}`)
	v.add(t, "certificates", `{"metadata":{"uid":"c2","name":"api-cert","namespace":"shop","creationTimestamp":"`+t0+`"},"spec":{"dnsNames":["api.example.org"],"secretName":"api-tls",
		"issuerRef":{"name":"letsencrypt","kind":"ClusterIssuer"}},"status":{"conditions":[{"type":"Ready","status":"False","message":"The certificate request has failed to complete"}]}}`)
	v.add(t, "ingresses", `{"metadata":{"uid":"i1","name":"shop","namespace":"shop"},"spec":{"tls":[{"hosts":["shop.example.org"],"secretName":"shop-tls"}],
		"rules":[{"host":"shop.example.org"}]}}`)
	v.add(t, "routes", `{"metadata":{"uid":"r1","name":"admin","namespace":"shop"},"spec":{"host":"admin.apps.example.org","tls":{"termination":"edge"}}}`)
	v.add(t, "routes", `{"metadata":{"uid":"r2","name":"grafana","namespace":"monitoring"},"spec":{"host":"grafana.apps.example.org","tls":{"termination":"edge"}}}`)
	v.add(t, "routes", `{"metadata":{"uid":"r3","name":"pay","namespace":"pay"},"spec":{"host":"pay.example.org","tls":`+
		js(map[string]string{"termination": "edge", "certificate": inlinePEM, "key": inlineKey})+`}}`)
	// Step 5: the same certificate inline on a host it doesn't cover.
	v.add(t, "routes", `{"metadata":{"uid":"r4","name":"old","namespace":"pay"},"spec":{"host":"old.example.com","tls":`+
		js(map[string]string{"termination": "edge", "certificate": inlinePEM, "key": inlineKey})+`}}`)
	v[tlsSeenRes] = map[string]any{
		"shop.example.org":         report.CertSeen{Addr: "shop.example.org", Port: 443, Cert: *shop},
		"admin.apps.example.org":   report.CertSeen{Addr: "admin.apps.example.org", Port: 443, Cert: *wild},
		"grafana.apps.example.org": report.CertSeen{Addr: "grafana.apps.example.org", Port: 443, Cert: *wild, Wrong: []string{"sent without its intermediate certificate"}},
	}

	if got := scanNames(v); strings.Join(got, " ") != "admin.apps.example.org grafana.apps.example.org shop.example.org" {
		t.Errorf("names tried: %q", got)
	}
	recs := build(v)
	b, _ := json.Marshal(recs)
	if strings.Contains(string(b), "shop-tls") || strings.Contains(string(b), "api-tls") {
		t.Error("a Secret's name left the cluster")
	}
	if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), strings.Split(inlineKey, "\n")[1]) {
		t.Fatal("a Route's key was kept")
	}
	got := byName(recs)
	if r := got["cert:shop.example.org"]; r.State != report.StateDegraded || r.Namespace != "shop" || r.Facts["fingerprint"] != shop.Fingerprint ||
		r.Facts["served_on"] != "shop/ingress/shop shop.example.org" || r.Facts["issued_by"] != "ClusterIssuer letsencrypt" || r.Facts["certificate"] != "shop-cert" ||
		!strings.HasPrefix(r.Word, "Not ready: Issuing certificate as Secret") || !strings.Contains(r.Word, "; ends in 9 days") {
		t.Errorf("cert-manager's, served: %+v", r)
	}
	if r := got["cert:api.example.org"]; r.State != report.StatePending || r.Word != "Not issued yet: The certificate request has failed to complete" {
		t.Errorf("not issued: %+v", r)
	}
	if r := got["cert:*.apps.example.org"]; r.State != report.StateDegraded ||
		r.Facts["wrong"] != "monitoring/route/grafana grafana.apps.example.org: sent without its intermediate certificate" ||
		!strings.HasSuffix(r.Word, "; served wrong: monitoring/route/grafana grafana.apps.example.org: sent without its intermediate certificate") || r.Namespace != "" || r.Facts["places"] != 2 || r.ID != "served/"+wild.Fingerprint[:16] ||
		r.Facts["served_on"] != "monitoring/route/grafana grafana.apps.example.org, shop/route/admin admin.apps.example.org" {
		t.Errorf("the router's wildcard: %+v", r)
	}
	if r := got["cert:pay.example.org"]; r.State != report.StateDegraded || r.Namespace != "pay" || r.Facts["places"] != 2 ||
		r.Facts["wrong"] != "pay/route/old old.example.com: doesn't cover old.example.com" || r.Facts["fingerprint"] != inline.Fingerprint || r.Facts["names"] != "pay.example.org" {
		t.Errorf("a Route's own: %+v", r)
	}
	if len(recs) != 9 { // four certificates, an Ingress and four Routes
		t.Errorf("%d records", len(recs))
	}
}
