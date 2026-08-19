// Package httpserver holds cross-handler HTTP concerns for /alerting.
// Deliberately duplicated from api/internal/httpserver rather than
// shared -- see /docs/phase-3-alerting-design.md's component-boundary
// note: no shared Go store/HTTP code between api and alerting, the same
// repo convention that only /proto is shared code.
package httpserver

import "net/http"

// WithCORS is deliberately permissive by default, same posture and
// reasoning as api's: no auth yet, the SvelteKit dev server runs on a
// different origin. Tighten alongside adding real auth.
func WithCORS(next http.Handler, allowedOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithCredentialedCORS is WithCORS's sibling for local login (see
// /docs -- deployment runbook): once alerting requires a session
// cookie/bearer token (sessioncheck.RequireSession), a browser calling
// it cross-origin must send credentials, and browsers categorically
// refuse to combine a credentialed request with
// Access-Control-Allow-Origin: "*" -- allowedOrigin must be a real,
// literal origin, not the wildcard WithCORS's own zero-config default
// relies on. Deliberately duplicated from api/httpserver's identical
// function rather than shared, same convention as WithCORS's own doc
// comment above.
func WithCredentialedCORS(next http.Handler, allowedOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
