// Package certinfo describes an X.509 certificate the same way wherever
// it's found (docs/cert-inventory.md): a site check's handshake, Vault's
// PKI, and later a host's ports and files. Public parts only.
package certinfo

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Of describes x.
func Of(x *x509.Certificate) *report.Cert {
	sum := sha256.Sum256(x.Raw)
	c := &report.Cert{Fingerprint: hex.EncodeToString(sum[:]), Subject: x.Subject.CommonName, Names: Names(x),
		Issuer: x.Issuer.CommonName, NotBefore: x.NotBefore.UTC(), NotAfter: x.NotAfter.UTC(), Key: Key(x), Sig: x.SignatureAlgorithm.String(), CA: x.IsCA}
	if c.Subject == "" && len(c.Names) > 0 {
		c.Subject = c.Names[0]
	}
	if c.Issuer == "" {
		c.Issuer = x.Issuer.String()
	}
	return c
}

// Names are what a certificate is for: its alternative names (DNS,
// addresses, URIs, emails), sorted.
func Names(x *x509.Certificate) []string {
	names := append([]string{}, x.DNSNames...)
	for _, ip := range x.IPAddresses {
		names = append(names, ip.String())
	}
	for _, u := range x.URIs {
		names = append(names, u.String())
	}
	names = append(names, x.EmailAddresses...)
	sort.Strings(names)
	return names
}

// Key is its public key in words: "RSA 2048", "ECDSA P-256", "Ed25519".
func Key(x *x509.Certificate) string {
	switch k := x.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + k.Curve.Params().Name
	case ed25519.PublicKey:
		return "Ed25519"
	}
	return x.PublicKeyAlgorithm.String()
}

// Weak says what's weak about it (decision 6), or "": SHA-1 or MD5
// signatures, RSA under 2048 bits. A root's own signature isn't checked:
// nothing verifies it.
func Weak(c *report.Cert) string {
	switch c.Sig {
	case "SHA1-RSA", "DSA-SHA1", "ECDSA-SHA1", "MD5-RSA", "MD2-RSA":
		if !(c.CA && c.Subject == c.Issuer) {
			return "signed with " + c.Sig
		}
	}
	var bits int
	if _, err := fmt.Sscanf(c.Key, "RSA %d", &bits); err == nil && bits < 2048 {
		return c.Key + " key"
	}
	return ""
}
