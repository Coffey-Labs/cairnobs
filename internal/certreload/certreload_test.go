package certreload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

type fakeSystem struct {
	can      bool
	reloaded []string
	fail     error
}

func (f *fakeSystem) CanReload(ctx context.Context, unit string) (bool, error) { return f.can, nil }
func (f *fakeSystem) Reload(ctx context.Context, unit string) error {
	f.reloaded = append(f.reloaded, unit)
	return f.fail
}

func makeCert(t *testing.T, names []string, end time.Time) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: end.Add(-90 * 24 * time.Hour), NotAfter: end}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	return x, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

func TestReload(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	dir := t.TempDir()
	names := []string{"www.example.com", "example.com"}
	old, _, _ := makeCert(t, names, now.Add(5*day))
	_, newPEM, newKey := makeCert(t, names, now.Add(85*day))
	le := filepath.Join(dir, "live", "www")
	_ = os.MkdirAll(le, 0o700)
	_ = os.WriteFile(filepath.Join(le, "fullchain.pem"), newPEM, 0o644)
	_ = os.WriteFile(filepath.Join(le, "privkey.pem"), newKey, 0o600)
	rules := []report.ReloadRule{{Unit: "nginx.service", Files: []string{dir + "/live/*/fullchain.pem"}}}
	req := Request{Unit: "nginx.service", Names: []string{"example.com", "WWW.example.com"}, Served: certinfo.Of(old).Fingerprint, Ends: old.NotAfter}

	sys := &fakeSystem{can: true}
	res, err := Do(context.Background(), rules, req, sys, now)
	if err != nil || len(sys.reloaded) != 1 || res.File != filepath.Join(le, "fullchain.pem") || !res.Ends.After(now.Add(80*day)) {
		t.Fatalf("%+v %v %v", res, err, sys.reloaded)
	}

	for _, c := range []struct {
		name  string
		setup func() (Request, []report.ReloadRule, *fakeSystem)
		want  string
	}{
		{"not listed", func() (Request, []report.ReloadRule, *fakeSystem) {
			r := req
			r.Unit = "sshd.service"
			return r, rules, &fakeSystem{can: true}
		}, "isn't in this host's cert_reload"},
		{"served is the newest", func() (Request, []report.ReloadRule, *fakeSystem) {
			r := req
			r.Ends = now.Add(100 * day)
			return r, rules, &fakeSystem{can: true}
		}, "no certificate for"},
		{"other names", func() (Request, []report.ReloadRule, *fakeSystem) {
			r := req
			r.Names = []string{"shop.example.com"}
			return r, rules, &fakeSystem{can: true}
		}, "no certificate for"},
		{"can't reload", func() (Request, []report.ReloadRule, *fakeSystem) { return req, rules, &fakeSystem{} }, "it isn't restarted"},
		{"reload fails", func() (Request, []report.ReloadRule, *fakeSystem) {
			return req, rules, &fakeSystem{can: true, fail: errors.New("Job failed")}
		}, "systemctl reload nginx.service: Job failed"},
	} {
		r, rs, s := c.setup()
		if _, err := Do(context.Background(), rs, r, s, now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		} else if c.name != "reload fails" && len(s.reloaded) != 0 {
			t.Errorf("%s: reloaded anyway", c.name)
		}
	}

	// A key that isn't the certificate's, then none.
	_, _, otherKey := makeCert(t, names, now.Add(day))
	_ = os.WriteFile(filepath.Join(le, "privkey.pem"), otherKey, 0o600)
	if _, err := Do(context.Background(), rules, req, &fakeSystem{can: true}, now); err == nil || !strings.Contains(err.Error(), "doesn't match its key") {
		t.Errorf("wrong key: %v", err)
	}
	_ = os.Remove(filepath.Join(le, "privkey.pem"))
	if _, err := Do(context.Background(), rules, req, &fakeSystem{can: true}, now); err == nil || !strings.Contains(err.Error(), "no private key found") {
		t.Errorf("no key: %v", err)
	}
	// haproxy's: key and certificate in one file.
	hp := filepath.Join(dir, "haproxy")
	_ = os.MkdirAll(hp, 0o700)
	_ = os.WriteFile(filepath.Join(hp, "site.pem"), append(newPEM, newKey...), 0o600)
	hr := []report.ReloadRule{{Unit: "haproxy.service", Files: []string{hp + "/**/*.pem"}}}
	r := req
	r.Unit = "haproxy.service"
	if _, err := Do(context.Background(), hr, r, &fakeSystem{can: true}, now); err != nil {
		t.Errorf("haproxy: %v", err)
	}
}

func TestReadRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	_ = os.WriteFile(p, []byte("server: https://x\nother: 1\ncert_reload:\n  - unit: nginx.service\n    files: [/etc/letsencrypt/live/*/fullchain.pem]\n"), 0o600)
	rs, err := ReadRules(p)
	if err != nil || len(rs) != 1 || rs[0].Unit != "nginx.service" || rs[0].Files[0] != "/etc/letsencrypt/live/*/fullchain.pem" {
		t.Fatalf("%+v %v", rs, err)
	}
	if err := validUnit("--now"); err == nil {
		t.Error("an option passed as a unit")
	}
}
