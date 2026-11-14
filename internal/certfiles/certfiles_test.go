package certfiles

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// mk makes a certificate signed by parent (itself when nil), and its key's
// PEM.
func mk(t *testing.T, cn string, ca bool, parent *x509.Certificate, pkey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), IsCA: ca, BasicConstraintsValid: true}
	if !ca {
		tpl.DNSNames = []string{cn}
	}
	if parent == nil {
		parent, pkey = tpl, k
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &k.PublicKey, pkey)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	kb, _ := x509.MarshalECPrivateKey(k)
	return x, k, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestExpand(t *testing.T) {
	got := Expand("/etc/nginx/**/*.{pem,crt}")
	if !slices.Equal(got, []string{"/etc/nginx/**/*.pem", "/etc/nginx/**/*.crt"}) {
		t.Errorf("%q", got)
	}
	if got := Expand("/a/{b,c}/{d,e}.pem"); len(got) != 4 || got[3] != "/a/c/e.pem" {
		t.Errorf("%q", got)
	}
	if got := Expand("/a/b.pem"); len(got) != 1 {
		t.Errorf("%q", got)
	}
}

// The helper's read: a leaf in a file with its chain, a leaf next to its
// key (haproxy's), a bundle of CAs and a key alone. Only the leaves come
// back, and no key's bytes.
func TestRead(t *testing.T) {
	dir := t.TempDir()
	ca, cak, caPEM, _ := mk(t, "Test CA", true, nil, nil)
	_, _, leafPEM, keyPEM := mk(t, "shop.example.org", false, ca, cak)
	_, _, mxPEM, mxKey := mk(t, "mx.example.org", false, ca, cak)
	_, _, _, aloneKey := mk(t, "nobody", false, ca, cak)
	write := func(name string, parts ...[]byte) string {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(strings.Join(func() []string {
			var s []string
			for _, b := range parts {
				s = append(s, string(b))
			}
			return s
		}(), "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	full := write("live/shop/fullchain.pem", leafPEM, caPEM)
	ha := write("haproxy/certs/deep/mx.pem", mxKey, mxPEM) // key first
	write("haproxy/bundle.pem", caPEM)
	write("haproxy/key.pem", aloneKey)
	write("haproxy/notes.txt", leafPEM)                         // not matched
	_, _, selfPEM, selfKey := mk(t, "snakeoil", true, nil, nil) // self-signed, marked a CA
	self := write("haproxy/self.pem", selfKey, selfPEM)
	block, _ := pem.Decode(leafPEM)
	der := write("pki/shop.crt", block.Bytes)

	r := &Reader{}
	got := r.Read([]string{dir + "/live/*/fullchain.pem", dir + "/haproxy/**/*.{pem,crt}", dir + "/pki/*.crt"})
	if len(got) != 4 {
		t.Fatalf("%d files: %+v", len(got), got)
	}
	byPath := map[string]int{}
	for i, f := range got {
		byPath[f.Path] = i
	}
	if f := got[byPath[full]]; f.Cert.Subject != "shop.example.org" || f.Chain != 1 || f.Cert.Issuer != "Test CA" || f.Mod.IsZero() {
		t.Errorf("fullchain: %+v", f)
	}
	if f := got[byPath[ha]]; f.Cert.Subject != "mx.example.org" || f.Chain != 0 {
		t.Errorf("haproxy's: %+v", f)
	}
	if f := got[byPath[self]]; f.Cert.Subject != "snakeoil" || !f.Cert.CA {
		t.Errorf("self-signed: %+v", f)
	}
	if f := got[byPath[der]]; f.Cert.Subject != "shop.example.org" {
		t.Errorf("DER: %+v", f)
	}
	b, _ := json.Marshal(got)
	for _, k := range [][]byte{keyPEM, mxKey, aloneKey, selfKey} {
		blk, _ := pem.Decode(k)
		if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), string(blk.Bytes[:16])) {
			t.Fatal("a key's bytes left the reader")
		}
	}

	// Unchanged files aren't parsed again; a changed one is.
	before := r.cache[full]
	_, _, renewed, _ := mk(t, "shop.example.org", false, ca, cak)
	later := time.Now().Add(time.Minute)
	_ = os.WriteFile(full, renewed, 0o600)
	_ = os.Chtimes(full, later, later)
	got = r.Read([]string{dir + "/live/*/fullchain.pem", dir + "/haproxy/**/*.{pem,crt}"})
	if len(got) != 3 || r.cache[full].file.Cert.Fingerprint == before.file.Cert.Fingerprint || r.cache[ha].file == nil {
		t.Errorf("after renewal: %+v", got)
	}
	if _, ok := r.cache[der]; ok {
		t.Error("a file no glob matches stays in the cache")
	}
}

func TestSkipped(t *testing.T) {
	if got := Find([]string{"/etc/ssl/certs/*.pem", "relative/*.pem"}); len(got) != 0 {
		t.Errorf("%q", got)
	}
}

// The agent sends the list on a change, and every half hour.
func TestWatch(t *testing.T) {
	var w Watch
	now := time.Now()
	if w.Report(now) != nil {
		t.Fatal("sent before the helper answered")
	}
	w.Set(nil)
	if r := w.Report(now); r == nil || r.List == nil {
		t.Fatal("an empty list is sent too")
	}
	if w.Report(now.Add(time.Minute)) != nil {
		t.Error("sent again unchanged")
	}
	if w.Report(now.Add(31*time.Minute)) == nil {
		t.Error("not resent")
	}
}

func TestMatch(t *testing.T) {
	globs := []string{"/etc/letsencrypt/live/*/fullchain.pem", "/etc/nginx/**/*.{pem,crt}"}
	for path, want := range map[string]bool{
		"/etc/letsencrypt/live/web/fullchain.pem":     true,
		"/etc/letsencrypt/live/web/sub/fullchain.pem": false,
		"/etc/letsencrypt/live/web/privkey.pem":       false,
		"/etc/nginx/ssl/a.crt":                        true,
		"/etc/nginx/a.pem":                            true,
		"/etc/nginx/a/b/c/d.pem":                      true,
		"/etc/nginxx/a.pem":                           false,
		"/etc/nginx/a.key":                            false,
		"/etc/haproxy/a.pem":                          false,
	} {
		if got := Match(globs, path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}
