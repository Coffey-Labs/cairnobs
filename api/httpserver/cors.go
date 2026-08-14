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
