package authz

import (
	"encoding/json"
	"net/http"
)

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

// RequireRole wraps next so it only runs for a caller whose resolved
// Identity.Role satisfies minRole. A nil authorizer is a deliberate,
// documented no-op -- a single-tenant deployment with no enterprise/
// configured behaves exactly as Phases 0-3 did, unauthenticated, not
// locked out. This is the same nil-safety shape as
// queryapi.AuditLogger and dashboards' optional dependencies.
func RequireRole(authorizer Authorizer, minRole Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authorizer == nil {
			next(w, r)
			return
		}
		identity, err := authorizer.Authorize(r)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		if !identity.Role.Satisfies(minRole) {
			writeForbidden(w)
			return
		}
		next(w, r.WithContext(withIdentity(r.Context(), identity)))
	}
}

// RequireRoleOrService is RequireRole plus an explicit allowance for
// RoleService -- used only by endpoints /alerting's evaluator legitimately
// calls (POST /query today). Every other endpoint uses plain RequireRole,
// so a service credential can never reach dashboard/rule administration
// even though it's a valid, authenticated identity -- narrow by default,
// widened only where a real machine caller exists.
func RequireRoleOrService(authorizer Authorizer, minRole Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authorizer == nil {
			next(w, r)
			return
		}
		identity, err := authorizer.Authorize(r)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		if identity.Role != RoleService && !identity.Role.Satisfies(minRole) {
			writeForbidden(w)
			return
		}
		next(w, r.WithContext(withIdentity(r.Context(), identity)))
	}
}
