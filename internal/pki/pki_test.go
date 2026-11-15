package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestCARoundTrip(t *testing.T) {
	ca := newCA(t)
	b, err := ca.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCA(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Fingerprint() != ca.Fingerprint() || !back.Key.Equal(ca.Key) {
		t.Fatal("CA changed in the round trip")
	}
	if !strings.HasPrefix(ca.Fingerprint(), "sha256:") || len(ca.Fingerprint()) != 7+64 {
		t.Fatalf("fingerprint %q", ca.Fingerprint())
	}
	if _, err := ParseCA(ca.CertPEM()); err == nil {
		t.Fatal("a certificate without its key parsed as a CA")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	ca := newCA(t)
	want := ca.Fingerprint()
	hexpart := strings.TrimPrefix(want, "sha256:")
	var colons []string
	for i := 0; i < len(hexpart); i += 2 {
		colons = append(colons, strings.ToUpper(hexpart[i:i+2]))
	}
	for _, in := range []string{want, hexpart, strings.ToUpper(want), " " + want + " ", strings.Join(colons, ":")} {
		got, err := NormalizeFingerprint(in)
		if err != nil || got != want {
			t.Errorf("NormalizeFingerprint(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "sha256:", "sha256:abcd", hexpart + "00", "md5:" + hexpart} {
		if _, err := NormalizeFingerprint(bad); err == nil {
			t.Errorf("NormalizeFingerprint(%q) accepted", bad)
		}
	}
}

func TestSignAgent(t *testing.T) {
	ca := newCA(t)
	_, csr, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	iss, err := ca.SignAgent(csr, "node1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(iss.PEM)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject.CommonName != "node1" {
		t.Errorf("common name %q, want the name obs-beacond gave, not the request's", c.Subject.CommonName)
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("usages %v, want client auth only", c.ExtKeyUsage)
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Error(err)
	}
	if SerialHex(c.SerialNumber) != iss.Serial || !c.NotAfter.Equal(iss.NotAfter.Truncate(time.Second)) {
		t.Error("Issued doesn't describe the certificate")
	}

	// Requests that must be refused.
	if _, err := ca.SignAgent([]byte("nonsense"), "node1", time.Now()); err == nil {
		t.Error("signed nonsense")
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rcsr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, rk)
	if _, err := ca.SignAgent(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: rcsr}), "node1", time.Now()); err == nil {
		t.Error("signed an RSA key")
	}
	// Same structure, one letter of the name changed: only the signature can catch it.
	cblk, _ := pem.Decode(csr)
	der := []byte(strings.Replace(string(cblk.Bytes), "obs-beacon-agent", "obs-beacon-agenT", 1))
	if string(der) == string(cblk.Bytes) {
		t.Fatal("test: name not found in the request")
	}
	tampered := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if _, err := ca.SignAgent(tampered, "node1", time.Now()); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("tampered request: %v, want a signature error", err)
	}
}

func TestVerifyPinned(t *testing.T) {
	ca, other := newCA(t), newCA(t)
	srv, err := ca.ServerCert([]string{"10.0.0.1", "beacon.example"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := VerifyPinned(srv.Certificate, ca.Fingerprint(), time.Now()); err != nil || got.Equal(ca.Cert) == false {
		t.Fatalf("good chain: %v", err)
	}
	if _, err := VerifyPinned(srv.Certificate, other.Fingerprint(), time.Now()); err == nil {
		t.Error("accepted a chain from a CA that isn't pinned")
	}
	if _, err := VerifyPinned(srv.Certificate[:1], ca.Fingerprint(), time.Now()); err == nil {
		t.Error("accepted a leaf without the CA in the chain")
	}
	if _, err := VerifyPinned(srv.Certificate, ca.Fingerprint(), time.Now().Add(ServerLifetime+time.Hour)); err == nil {
		t.Error("accepted an expired server certificate")
	}
	// The pinned CA in the chain, but a leaf from another CA.
	stray, _ := other.ServerCert(nil, time.Now())
	if _, err := VerifyPinned([][]byte{stray.Certificate[0], ca.Cert.Raw}, ca.Fingerprint(), time.Now()); err == nil {
		t.Error("accepted a leaf the pinned CA didn't sign")
	}
	// An agent's certificate can't pose as the server.
	_, csr, _ := NewKey()
	iss, _ := ca.SignAgent(csr, "node1", time.Now())
	blk, _ := pem.Decode(iss.PEM)
	if _, err := VerifyPinned([][]byte{blk.Bytes, ca.Cert.Raw}, ca.Fingerprint(), time.Now()); err == nil {
		t.Error("accepted an agent certificate as the server's")
	}
	// A server certificate can't be used as an agent's.
	if _, err := srv.Leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Error("the server certificate passes as a client certificate")
	}
}
