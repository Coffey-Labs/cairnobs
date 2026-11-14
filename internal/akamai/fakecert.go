package akamai

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// FakePEM makes a certificate for the fake's enrollments: self-signed
// with an issuer name of its choosing, RSA 2048 or ECDSA P-256, with the
// given names and ending at end, after a 90-day life where that has
// begun. For the tests and
// the fleet simulator only.
func FakePEM(issuer string, names []string, ecdsaKey bool, end time.Time) string {
	var pub any
	var priv crypto.Signer
	if ecdsaKey {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		pub, priv = &k.PublicKey, k
	} else {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		pub, priv = &k.PublicKey, k
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	begin := end.Add(-90 * 24 * time.Hour)
	if now := time.Now(); begin.After(now) {
		begin = now.Add(-30 * 24 * time.Hour) // a longer-lived one
	}
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names, NotBefore: begin, NotAfter: end}
	parent := &x509.Certificate{Subject: pkix.Name{CommonName: issuer}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, pub, priv)
	if err != nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
