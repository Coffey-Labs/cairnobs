// Package tlspin is how the platform and array readers trust what they
// talk to: a CA file, a pinned certificate, or the system's CAs.
package tlspin

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNotPinned is a certificate that isn't the pinned one.
var ErrNotPinned = errors.New("the certificate isn't the pinned one")

// Config trusts the certificate whose SHA-256 is fingerprint (hex, colons
// or not, any case) and nothing else, whatever names it holds; else the CA
// in caFile (PEM); else the system's CAs.
func Config(caFile, fingerprint string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch fp := strings.ToLower(strings.ReplaceAll(fingerprint, ":", "")); {
	case fp != "":
		want, err := hex.DecodeString(fp)
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("fingerprint is a SHA-256, in hex")
		}
		tc.InsecureSkipVerify = true // the pin below is the check
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) > 0 {
				if sum := sha256.Sum256(raw[0]); string(sum[:]) == string(want) {
					return nil
				}
			}
			return ErrNotPinned
		}
	case caFile != "":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificate in %s", caFile)
		}
		tc.RootCAs = pool
	}
	return tc, nil
}
