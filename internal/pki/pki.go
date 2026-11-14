// Package pki is the agent link's own small certificate authority.
//
// obs-beacond makes one CA per installation on its first start and keeps it
// in its database. It signs two kinds of certificate and nothing else:
//
//   - the agent listener's server certificate (ExtKeyUsageServerAuth),
//     made in memory at start and again before it runs out;
//   - one client certificate per agent (ExtKeyUsageClientAuth), its
//     common name the agent's name, signed from the agent's own request.
//
// Agents pin the CA by the SHA-256 of its certificate. They check the
// server's chain against that CA and its server usage, but not the host
// name: the pinned CA is this installation's alone and the listener's
// certificate is the only server certificate it ever signs, so a name check
// adds nothing and would break agents behind NAT or reaching the server by
// an address nobody listed. An agent's certificate can't pose as the server,
// because it carries only the client usage.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// Lifetimes. Agents renew once two thirds of theirs has passed.
const (
	CALifetime     = 10 * 365 * 24 * time.Hour
	AgentLifetime  = 90 * 24 * time.Hour
	ServerLifetime = 90 * 24 * time.Hour
	backdate       = 5 * time.Minute // clocks a little behind still accept a fresh certificate
)

// CA is the installation's agent CA.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA makes a new CA.
func NewCA(now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Cairn OBS agent CA", Organization: []string{"Cairn OBS"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CALifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Marshal is the CA as PEM: certificate, then key. Seal it before storing.
func (ca *CA) Marshal() ([]byte, error) {
	k, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, err
	}
	return append(ca.CertPEM(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})...), nil
}

// ParseCA reverses Marshal.
func ParseCA(b []byte) (*CA, error) {
	var ca CA
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, err
			}
			ca.Cert = c
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, err
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, errors.New("CA key is not ECDSA")
			}
			ca.Key = ek
		}
	}
	if ca.Cert == nil || ca.Key == nil || !ca.Cert.IsCA {
		return nil, errors.New("not a CA certificate and key")
	}
	return &ca, nil
}

// CertPEM is the CA certificate alone, as PEM.
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
}

// Fingerprint is what agents pin: "sha256:" and the hex SHA-256 of the
// CA certificate.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert) }

// Fingerprint of any certificate, in the same form.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts "sha256:<hex>", bare hex, or hex with
// colons, any case, and returns the canonical form.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	if b, err := hex.DecodeString(s); err != nil || len(b) != sha256.Size {
		return "", errors.New("a CA fingerprint is sha256: and 64 hex characters")
	}
	return "sha256:" + s, nil
}

// ServerCert makes the agent listener's certificate. names are host names
// and IP addresses for its SANs; agents don't check them (see the package
// comment), they're there for curl and other tools.
func (ca *CA) ServerCert(names []string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "obs-beacond agent listener"},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(ServerLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	// The chain carries the CA so an agent that only knows its fingerprint
	// can check the leaf against it.
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// Issued is a signed agent certificate.
type Issued struct {
	PEM      []byte
	Serial   string // hex
	NotAfter time.Time
}

// CheckAgentCSR parses an agent's certificate request and checks its
// signature and key, without signing anything.
func (ca *CA) CheckAgentCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("request signature: %w", err)
	}
	switch k := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, errors.New("ECDSA keys must be P-256 or P-384")
		}
	default:
		return nil, errors.New("agent keys must be ECDSA")
	}
	return csr, nil
}

// SignAgent signs an agent's certificate request. The common name is the
// agent's name from obs-beacond's own records, whatever the request says;
// the request only supplies the public key (and proves the agent holds it).
func (ca *CA) SignAgent(csrPEM []byte, agent string, now time.Time) (Issued, error) {
	csr, err := ca.CheckAgentCSR(csrPEM)
	if err != nil {
		return Issued{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return Issued{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agent},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(AgentLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return Issued{}, err
	}
	return Issued{
		PEM:      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Serial:   SerialHex(serial),
		NotAfter: tmpl.NotAfter,
	}, nil
}

// Pool is a cert pool holding just the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// SerialHex is how serials are stored and compared.
func SerialHex(n *big.Int) string { return hex.EncodeToString(n.Bytes()) }

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// NewKey makes an agent key and a certificate request for it, both PEM.
func NewKey() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "obs-beacon-agent"}, // obs-beacond sets the real name
	}, crypto.Signer(key))
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}

// VerifyPinned checks a server's chain the way agents do: some certificate
// in it must be the pinned CA, and the leaf must chain to that CA for
// server use. It returns the CA certificate it found.
func VerifyPinned(rawCerts [][]byte, fingerprint string, now time.Time) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, errors.New("server sent no certificate")
	}
	var certs []*x509.Certificate
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	var ca *x509.Certificate
	for _, c := range certs[1:] {
		if Fingerprint(c) == fingerprint {
			ca = c
		}
	}
	if ca == nil {
		return nil, fmt.Errorf("server's CA is not the pinned one (%s): wrong server, or ca_fingerprint is out of date", fingerprint)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	return ca, nil
}
