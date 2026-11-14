package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// TestRealPKI issues certificates on a PKI mount of the dev-mode Vault
// and OpenBao, and reads them with the monitoring token: one record per
// set of names, the newest counting; short-lived ones counted, revoked
// ones not; judged by the time left.
func TestRealPKI(t *testing.T) {
	addrs := os.Getenv("CAIRN_OBS_TEST_VAULT")
	if addrs == "" {
		t.Skip("CAIRN_OBS_TEST_VAULT not set")
	}
	for _, base := range strings.Split(addrs, ",") {
		t.Run(base, func(t *testing.T) {
			mount := fmt.Sprintf("pkit%d", time.Now().UnixNano()%1e9)
			root := func(method, path string, body, v any) { rootCall(t, base, "root-ci", method, path, body, v) }
			root(http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "pki", "config": map[string]any{"max_lease_ttl": "87600h"}}, nil)
			t.Cleanup(func() { root(http.MethodDelete, "sys/mounts/"+mount, nil, nil) })
			root(http.MethodPost, mount+"/root/generate/internal", map[string]any{"common_name": "Test Root", "ttl": "87600h", "issuer_name": "root-test", "key_type": "ec", "not_before_duration": "2500h"}, nil)
			root(http.MethodPost, mount+"/roles/web", map[string]any{"allow_any_name": true, "enforce_hostnames": false, "max_ttl": "8760h", "key_type": "ec"}, nil)
			// "old" backdates what it issues 100 days, so a certificate
			// has lived most of its life the moment it's made.
			root(http.MethodPost, mount+"/roles/old", map[string]any{"allow_any_name": true, "enforce_hostnames": false, "max_ttl": "8760h", "key_type": "ec", "not_before_duration": "2400h"}, nil)
			issue := func(role, cn, ttl string) string {
				var r struct {
					Data struct {
						Serial string `json:"serial_number"`
					} `json:"data"`
				}
				root(http.MethodPost, mount+"/issue/"+role, map[string]any{"common_name": cn, "ttl": ttl}, &r)
				return r.Data.Serial
			}
			issue("web", "a.example.org", "720h")
			issue("web", "a.example.org", "721h") // renewed: one record, two certificates
			issue("web", "b.example.org", "240h")
			issue("old", "c.example.org", "360h") // 15 of 115 days left: a warning
			issue("old", "d.example.org", "120h") // 5 days left: critical
			issue("web", "job.example.org", "1h") // short-lived: counted
			gone := issue("web", "e.example.org", "720h")
			root(http.MethodPost, mount+"/revoke", map[string]any{"serial_number": gone}, nil)

			var made struct {
				Auth struct {
					ClientToken string `json:"client_token"`
				} `json:"auth"`
			}
			root(http.MethodPut, "sys/policies/acl/obs-monitor", map[string]any{"policy": policy}, nil)
			root(http.MethodPost, "auth/token/create", map[string]any{"policies": []string{"obs-monitor"}, "ttl": "1h"}, &made)
			c, _ := newClient(Config{Name: "dev", URL: base, TokenFile: writeToken(t, made.Auth.ClientToken), MinLifetime: 7 * 24 * time.Hour})
			for read := range 2 { // the second from the cache
				s, err := c.Read(context.Background(), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if len(s.Problems) > 0 {
					t.Fatalf("problems %v", s.Problems)
				}
				byName := map[string]report.Workload{}
				var part *report.Workload
				for _, r := range Records(s) {
					switch {
					case r.ID == PKIPart+mount:
						part = &r
					case r.Kind == report.KindCert && r.Namespace == mount:
						byName[r.Name] = r
					}
				}
				if part == nil || part.State != report.StateOK || part.Facts["revoked"] != 1 || part.Facts["short_lived"] != 1 || part.Facts["current"] != 4 || part.Facts["pending"] != 0 {
					t.Fatalf("read %d: mount %+v", read, part)
				}
				for name, want := range map[string]string{"Test Root": report.StateOK, "a.example.org": report.StateOK, "b.example.org": report.StateOK,
					"c.example.org": report.StateDegraded, "d.example.org": report.StateDown} {
					if r, ok := byName[name]; !ok || r.State != want {
						t.Errorf("read %d: %s: %+v, want %s", read, name, r, want)
					}
				}
				if len(byName) != 5 || byName["a.example.org"].Facts["certificates"] != 2 || byName["Test Root"].Facts["ca"] != true || byName["Test Root"].Facts["issuer_name"] != "root-test" {
					t.Errorf("read %d: %v", read, byName)
				}
			}
		})
	}
}

// testCA makes a CA and certificates it signs, for the fake.
type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pem  string
	n    int64
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake Root"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{key: key, cert: c, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), n: 1}
}

func (ca *testCA) issue(t *testing.T, cn string, from, to time.Time) (serial, pemText string) {
	t.Helper()
	ca.n++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(ca.n), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}, NotBefore: from, NotAfter: to}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	return serialOf(x), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A mount with more certificates than one read may fetch is read over
// several; each certificate is fetched once; one tidied away is dropped.
func TestPKIBudget(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	certs := map[string]string{}
	var order []string
	for i := range 5 {
		sn, p := ca.issue(t, fmt.Sprintf("s%d.example.org", i), now.Add(-time.Hour), now.Add(90*24*time.Hour))
		certs[sn] = p
		order = append(order, sn)
	}
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch p := strings.TrimPrefix(r.URL.Path, "/v1/"); {
		case p == "sys/health":
			v = map[string]any{"initialized": true, "sealed": false, "version": "1.21.4"}
		case p == "sys/leader":
			v = map[string]any{"ha_enabled": false}
		case p == "auth/token/lookup-self":
			v = map[string]any{"data": map[string]any{"ttl": 0}}
		case p == "sys/mounts":
			v = map[string]any{"data": map[string]any{"pki/": map[string]any{"type": "pki"}, "secret/": map[string]any{"type": "kv"}}}
		case p == "pki/issuers":
			v = map[string]any{"data": map[string]any{"keys": []string{"i1"}}}
		case p == "pki/issuer/i1":
			v = map[string]any{"data": map[string]any{"certificate": ca.pem, "issuer_name": "fake"}}
		case p == "pki/certs":
			keys := []string{serialOf(ca.cert)}
			for _, sn := range order {
				keys = append(keys, sn)
			}
			v = map[string]any{"data": map[string]any{"keys": keys}}
		case strings.HasPrefix(p, "pki/cert/"):
			fetches.Add(1)
			v = map[string]any{"data": map[string]any{"certificate": certs[strings.TrimPrefix(p, "pki/cert/")]}}
		default:
			w.WriteHeader(http.StatusNotFound)
			v = map[string]any{"errors": []string{}}
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()
	old := fetchBudget
	fetchBudget = 2
	defer func() { fetchBudget = old }()
	c, _ := newClient(Config{Name: "fake", URL: srv.URL, TokenFile: writeToken(t, "t"), MinLifetime: 7 * 24 * time.Hour})
	for i, want := range []int{3, 1, 0, 0} {
		s, err := c.Read(context.Background(), time.Now())
		if err != nil || len(s.PKI) != 1 {
			t.Fatalf("read %d: %v %+v", i, err, s)
		}
		if m := s.PKI[0]; m.Pending != want || len(m.Certs) != 5-want || m.Err != "" || len(m.Issuers) != 1 {
			t.Errorf("read %d: pending %d, %d certs, err %q", i, m.Pending, len(m.Certs), m.Err)
		}
	}
	if n := fetches.Load(); n != 5 {
		t.Errorf("fetched %d times, want 5", n)
	}
	order = order[1:] // tidied
	s, _ := c.Read(context.Background(), time.Now())
	if len(s.PKI[0].Certs) != 4 || len(c.certs["pki"]) != 4 {
		t.Errorf("after tidy: %d certs, %d cached", len(s.PKI[0].Certs), len(c.certs["pki"]))
	}
	part := Records(s)[2] // the cluster, the token, then the mount
	if part.ID != "pki/pki" || part.Word != "4 names with a current certificate" {
		t.Errorf("mount %+v", part)
	}
}

// Thresholds: a third or a tenth of a short life, else 21 and 7 days; an
// ended one stays 7 days unless renewed; short-lived ones aren't judged.
func TestPKIJudged(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cert := func(cn string, lived, left time.Duration) Cert {
		return Cert{Serial: cn + lived.String() + left.String(), Subject: cn, Names: cn, NotBefore: now.Add(-lived), NotAfter: now.Add(left)}
	}
	m := Mount{Path: "pki", Listed: 9, Certs: []Cert{
		cert("year", 300*day, 65*day),        // fine
		cert("warn", 300*day, 20*day),        // under 21 days
		cert("crit", 300*day, 6*day),         // under 7
		cert("month", 20*day, 10*day),        // 30-day life: warns under 10 days, critical under 3
		cert("month2", 28*day, 2*day),        // critical
		cert("ended", 400*day, -2*day),       // ended 2 days ago: critical
		cert("long-gone", 400*day, -9*day),   // ended 9 days ago: dropped
		cert("renewed", 400*day, -2*day),     // ended, but…
		cert("renewed", 5*day, 360*day),      // …renewed
		cert("hourly", time.Hour, time.Hour), // short-lived
	}}
	s := &Snapshot{At: now, MinLife: 7 * day, PKI: []Mount{m}}
	got := map[string]string{}
	for _, r := range pkiRecords(s) {
		got[r.Name] = r.State + ": " + r.Word
	}
	want := map[string]string{
		"pki/pki": report.StateOK + ": 7 names with a current certificate; 5 end within 21 days; 1 short-lived certificate, not judged",
		"year":    report.StateOK + ": Ends in 65 days (2027-01-17)",
		"warn":    report.StateDegraded + ": Ends in 20 days (2026-12-03)",
		"crit":    report.StateDown + ": Ends in 6 days (2026-11-19)",
		"month":   report.StateOK + ": Ends in 10 days (2026-11-23)",
		"month2":  report.StateDown + ": Ends in 2 days (2026-11-15)",
		"ended":   report.StateDown + ": Ended 2 days ago (2026-11-11)",
		"renewed": report.StateOK + ": Ends in 360 days (2027-11-08)",
	}
	if len(got) != len(want) {
		t.Errorf("records %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %q, want %q", k, got[k], w)
		}
	}
}
