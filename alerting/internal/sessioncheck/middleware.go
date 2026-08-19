package sessioncheck

import (
	"encoding/json"
	"net/http"
)

// sessionCookieName must match api/localauth's sessionCookieName
// exactly (unexported there too, deliberately duplicated rather than
// imported -- see this package's doc comment) -- the same cookie
// api/localauth.Handler.setCookie writes, scoped (via SESSION_COOKIE_
// DOMAIN) to cover both api's and alerting's subdomains in production.
const sessionCookieName = "sentry_local_session"

type errorResponse struct {
	Error string `json:"error"`
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: "unauthorized"})
}

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: "forbidden"})
}

// mutatingRoleFloor is the minimum role RequireSession enforces for any
// non-read request -- closes a real gap the security audit found: this
// package used to be a pure "logged in or not" gate with no role check
// at all, meaning a Viewer-role session could create/delete alert rules
// and notification targets exactly like an Editor. GET/HEAD (read-only)
// stay at "any valid session," matching every role floor in this
// codebase's other RBAC-gated resources (queries, dashboards) using
// Viewer as their read bar.
const mutatingRoleFloor = "editor"

func isReadOnly(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func credentialFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return auth[len(prefix):]
		}
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// RequireSession wraps next so every request needs a valid local-login
// session -- a blanket gate, not per-route roles: alerting has no
// role-check plumbing at all today (unlike api/authz's per-route
// RequireRole), and building a full parallel system just for this
// feature is out of scope (see /docs/agent-management-design.md-style
// "resist scope creep" discipline this codebase applies everywhere).
// GET /healthz is deliberately exempt -- Docker's HEALTHCHECK execs
// this same binary against itself over loopback (cmd/alerting/main.go's
// runHealthcheck), pre-auth, and must keep working regardless of
// whether local auth is enabled.
func RequireSession(checker *Checker, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		raw := credentialFromRequest(r)
		if raw == "" {
			writeUnauthorized(w)
			return
		}
		role, err := checker.Validate(r.Context(), raw)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		if !isReadOnly(r.Method) && !roleSatisfies(role, mutatingRoleFloor) {
			writeForbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
