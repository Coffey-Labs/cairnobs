package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeAuthorizer struct {
	identity Identity
	err      error
}

func (f *fakeAuthorizer) Authorize(_ *http.Request) (Identity, error) {
	return f.identity, f.err
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFromContext(r.Context())
	if ok {
		w.Header().Set("X-Test-Tenant", id.TenantID)
	}
	w.WriteHeader(http.StatusOK)
}

func TestRequireRoleNilAuthorizerIsNoOp(t *testing.T) {
	h := RequireRole(nil, RoleAdmin, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil authorizer must be a no-op, matching single-tenant Phase 0-3 behavior)", rec.Code)
	}
}

func TestRequireRoleRejectsUnauthenticated(t *testing.T) {
	h := RequireRole(&fakeAuthorizer{err: errors.New("no session")}, RoleViewer, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequireRoleRejectsInsufficientRole(t *testing.T) {
	h := RequireRole(&fakeAuthorizer{identity: Identity{Role: RoleViewer}}, RoleAdmin, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRequireRoleAllowsSufficientRoleAndAttachesIdentity(t *testing.T) {
	h := RequireRole(&fakeAuthorizer{identity: Identity{TenantID: "acme", Role: RoleAdmin}}, RoleEditor, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Test-Tenant"); got != "acme" {
		t.Fatalf("expected the resolved identity to be attached to the request context, got tenant=%q", got)
	}
}

func TestRequireRoleOrServiceAllowsServiceIdentity(t *testing.T) {
	h := RequireRoleOrService(&fakeAuthorizer{identity: Identity{Role: RoleService}}, RoleAdmin, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- RoleService must satisfy RequireRoleOrService regardless of minRole", rec.Code)
	}
}

func TestRequireRoleOrServiceStillRejectsInsufficientHumanRole(t *testing.T) {
	h := RequireRoleOrService(&fakeAuthorizer{identity: Identity{Role: RoleViewer}}, RoleAdmin, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- allowing RoleService must not loosen the human-role check", rec.Code)
	}
}

// TestRequireRolePlainDoesNotAllowService pins down the narrow-by-default
// property middleware.go's doc comment claims: an endpoint using plain
// RequireRole (not RequireRoleOrService) must reject a RoleService
// identity even if the minRole would technically be satisfiable on the
// human scale -- RoleService never satisfies anything but itself.
func TestRequireRolePlainDoesNotAllowService(t *testing.T) {
	h := RequireRole(&fakeAuthorizer{identity: Identity{Role: RoleService}}, RoleViewer, okHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- plain RequireRole must never admit a service identity", rec.Code)
	}
}
