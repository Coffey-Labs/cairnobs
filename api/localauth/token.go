package localauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// newOpaqueToken returns a fresh session credential: raw is what's set
// in the cookie/returned to the caller (base64url, URL/cookie-safe),
// hash is what's stored in local_sessions.token_hash. Only the hash is
// ever persisted -- same reasoning 0034_create_ingest_credentials.sql
// gives for hashing its own bearer tokens: the server only ever needs
// to check "does the presented value match," never recover the raw
// value.
func newOpaqueToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashToken(raw), nil
}

// hashToken re-derives a token's hash from a raw value a caller
// presents (Authorization header or cookie), for lookup against
// local_sessions.token_hash. Plain SHA-256, not bcrypt: unlike a
// password, a session token is already high-entropy random data, not
// something an attacker could feasibly brute-force offline even from a
// leaked hash, so there's no need for bcrypt's deliberate slowness here
// -- alerting/internal/sessioncheck validates sessions on every request
// and does the same plain hash, with no bcrypt dependency at all.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
