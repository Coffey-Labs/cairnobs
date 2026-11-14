// Package jointoken is the one-time token a new host joins with: everything
// obs-beacon-agent needs to reach obs-beacond's agent listener and trust it,
// in one string an admin pastes.
//
// A token carries the agent listener's URL, the agent CA's fingerprint (the
// agent pins it, as with ca_fingerprint) and a random secret. obs-beacond
// keeps only the secret's SHA-256; the secret is good once, for a short
// while, and only for POST /api/v1/agent/join. The token is not encrypted:
// whoever reads it can join a host under its name until it is used or runs
// out, so it is shown once and not logged.
package jointoken

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cairnobs/cairnobs/internal/pki"
)

// prefix marks a join token and its version.
const prefix = "cobsj1_"

// Token is a join token's contents.
type Token struct {
	Server        string `json:"u"` // the agent listener, https://host:port
	CAFingerprint string `json:"f"` // sha256:<hex>
	Secret        string `json:"s"`
}

// Encode makes the string form.
func (t Token) Encode() string {
	b, _ := json.Marshal(t)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Parse reads and checks a token.
func Parse(s string) (Token, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok {
		return Token{}, errors.New("not a join token (they start with " + prefix + ")")
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return Token{}, fmt.Errorf("join token: %w (was it cut short when copied?)", err)
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, fmt.Errorf("join token: %w", err)
	}
	u, err := url.Parse(t.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Token{}, fmt.Errorf("join token: server %q is not an https URL", t.Server)
	}
	if t.CAFingerprint, err = pki.NormalizeFingerprint(t.CAFingerprint); err != nil {
		return Token{}, fmt.Errorf("join token: %w", err)
	}
	if len(t.Secret) < 32 {
		return Token{}, errors.New("join token: no secret")
	}
	return t, nil
}
