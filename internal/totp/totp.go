// Package totp is RFC 6238 time-based one-time codes as authenticator apps
// make them: HMAC-SHA1, 30-second steps, six digits.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	period = 30
	digits = 6
	// skew is how many steps either side of now a code is still taken: a
	// phone's clock a little off, or a code typed as it changed.
	skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret makes a 160-bit secret, base32 as apps want it.
func NewSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// Step is the time step t falls in.
func Step(t time.Time) int64 { return t.Unix() / period }

// Code is the code for one step.
func Code(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("totp secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, n%1_000_000), nil
}

// Verify checks a code against now, and that its step is later than
// after (the last step used), so a code works once. It returns the step
// the code matched.
func Verify(secret, code string, now time.Time, after int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits {
		return 0, false
	}
	cur := Step(now)
	for s := cur - skew; s <= cur+skew; s++ {
		if s <= after {
			continue
		}
		want, err := Code(secret, s)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// URI is the otpauth:// link authenticator apps read (as a QR code or a
// tapped link).
func URI(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("period", fmt.Sprint(period))
	v.Set("digits", fmt.Sprint(digits))
	v.Set("algorithm", "SHA1")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}
