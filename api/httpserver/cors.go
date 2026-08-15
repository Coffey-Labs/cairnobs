// Package httpserver holds cross-handler HTTP concerns for /api. Phase 3
// introduced a second handler package (dashboards) alongside
// queryapi, so CORS moved out of individual handlers into one
// wrap applied around the fully-assembled mux in cmd/api/main.go, rather
// than each handler package wrapping itself.
package httpserver

import "net/http"

// WithCORS is deliberately permissive by default (see CORSAllowedOrigin
// in internal/config) since there's no auth yet and the SvelteKit dev
// server runs on a different origin. Tighten alongside adding real auth.
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

// WithCredentialedCORS is WithCORS's sibling for endpoints a browser must
// call with cookies attached (Phase 4: enterprise-auth's
// GET /auth/memberships / POST /auth/select-tenant, called from web's
// tenant-picker page via `fetch(..., {credentials: 'include'})`).
// Browsers categorically refuse to combine a credentialed request with
// Access-Control-Allow-Origin: "*" -- allowedOrigin must be a real,
// literal origin (e.g. "http://localhost:3000"), not the wildcard
// WithCORS's own zero-config default relies on. Callers of this function
// don't get that convenient zero-config default: an empty/wildcard
// allowedOrigin here is a configuration bug, not a permissive default,
// so it's deliberately not special-cased into something that "just
// works" the way plain WithCORS's "*" does.
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
