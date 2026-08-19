package localauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter is a simple in-memory sliding-window rate limiter for
// POST /auth/login, keyed by client IP -- closes a real gap the
// security audit found: nothing in the application, nginx, or the host
// (no fail2ban either) throttled repeated login attempts, making
// sustained online brute-forcing of a weaker, human-chosen password
// possible. (The auto-generated admin password is high-entropy, but
// every user created afterward only needs 8 characters with no
// complexity check -- see handleCreateUser.)
//
// Per-IP rather than per-username: a per-username-only limiter is
// itself a denial-of-service vector (deliberately fail a real
// username's login repeatedly, from anywhere, to lock them out), and
// wouldn't bound an attacker guessing across many usernames from one
// source. Both successful and failed attempts count against the
// window, not just failures -- simpler, and it means a low-and-slow
// guesser can't reset their budget by occasionally succeeding against
// an unrelated account.
//
// Deliberately in-memory, not Postgres-backed: login rate limiting is
// inherently best-effort per-process state (a restart clearing it is
// fine, unlike a session or password), and adding a database
// round-trip to every login attempt is the wrong tradeoff for a check
// whose only job is bounding attempt *rate*. Memory for IPs that stop
// attempting entirely is only reclaimed the next time that exact key is
// looked up -- a deliberate, bounded-in-practice simplicity tradeoff
// (real attacker/user IP cardinality against one deployment is small
// relative to a process's lifetime between deploys), not an oversight.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
	window   time.Duration
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{attempts: map[string][]time.Time{}, max: max, window: window}
}

// allow reports whether key may attempt another login right now, and
// records this attempt if so (a denied call does not itself count as a
// new attempt -- it just reports the existing window is full).
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)
	var kept []time.Time
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)
	return true
}

// clientIP extracts the caller's address for rate-limiting purposes.
// Trusts the first hop of X-Forwarded-For when present -- correct for
// this deployment's actual topology (always behind nginx, which sets
// it), but note this is spoofable by any caller that reaches the
// application directly rather than through the trusted proxy; a
// deployment that exposes api's port directly to untrusted clients
// should not rely on this header. Falls back to r.RemoteAddr, which is
// always accurate for whoever the TCP connection is actually with.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
