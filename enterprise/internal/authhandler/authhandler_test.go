package authhandler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cairnobs/cairnobs/enterprise/internal/session"
)

// fakeIngestCredentialValidator is an in-memory stand-in for
// *rbacstore.Store's ValidateIngestCredential, keyed by token.
type fakeIngestCredentialValidator struct {
	tenantByToken map[string]string
}

func newFakeIngestCredentialValidator() *fakeIngestCredentialValidator {
	return &fakeIngestCredentialValidator{tenantByToken: map[string]string{}}
}

func (f *fakeIngestCredentialValidator) ValidateIngestCredential(_ context.Context, token string) (string, error) {
	tenantID, ok := f.tenantByToken[token]
	if !ok {
		return "", errNotFound
	}
	return tenantID, nil
}

var errNotFound = &fakeNotFoundError{}

type fakeNotFoundError struct{}

func (*fakeNotFoundError) Error() string { return "not found" }

// fakeTenantLister is an in-memory stand-in for *rbacstore.Store's
// ListActiveTenantIDs.
type fakeTenantLister struct {
	ids []string
	err error
}

func (f *fakeTenantLister) ListActiveTenantIDs(_ context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ids, nil
}

func testHandler(t *testing.T) (*Handler, *session.Manager) {
	t.Helper()
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{}, newFakeIngestCredentialValidator(), &fakeTenantLister{}), m
}

func doAuthorize(t *testing.T, h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorize", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAuthorizeViaServiceToken(t *testing.T) {
	h, m := testHandler(t)
	token, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Role != "service" || body.TenantID != "" || body.UserID != "" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestAuthorizeViaSessionCookie(t *testing.T) {
	h, m := testHandler(t)
	token, err := m.IssueUserSession("acme", "u1", "editor")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.TenantID != "acme" || body.UserID != "u1" || body.Role != "editor" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestAuthorizeBearerTakesPrecedenceOverCookie(t *testing.T) {
	h, m := testHandler(t)
	serviceToken, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	sessionToken, err := m.IssueUserSession("acme", "u1", "viewer")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+serviceToken)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
	})
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Role != "service" {
		t.Fatalf("expected the Bearer service token to win, got role %q", body.Role)
	}
}

func TestAuthorizeNoCredentialsIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorize(t, h, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizeInvalidTokenIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-real-token")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFeaturesReflectsConfiguredMechanisms(t *testing.T) {
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{OIDCEnabled: true, SAMLEnabled: false}, newFakeIngestCredentialValidator(), &fakeTenantLister{})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/features", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body featuresResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !body.SSOConfigured || !body.OIDCEnabled || body.SAMLEnabled {
		t.Fatalf("unexpected features response: %+v", body)
	}
}

func TestFeaturesAllFalseWhenNothingConfigured(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/features", nil))

	var body featuresResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.SSOConfigured || body.OIDCEnabled || body.SAMLEnabled {
		t.Fatalf("expected all-false features when nothing is configured, got %+v", body)
	}
}

func TestAuthorizeTokenFromWrongManagerIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	otherManager, err := session.NewManager([]byte("a-completely-different-32-byte-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	token, err := otherManager.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func doAuthorizeIngest(t *testing.T, h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorize-ingest", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAuthorizeIngestResolvesTenant(t *testing.T) {
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	validator := newFakeIngestCredentialValidator()
	validator.tenantByToken["real-token"] = "acme"
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{}, validator, &fakeTenantLister{})

	rec := doAuthorizeIngest(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer real-token")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body authorizeIngestResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.TenantID != "acme" {
		t.Fatalf("TenantID = %q, want acme", body.TenantID)
	}
}

func TestAuthorizeIngestNoCredentialsIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorizeIngest(t, h, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizeIngestUnknownTokenIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorizeIngest(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-real-token")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAuthorizeIngestRejectsSessionToken is the regression test for the
// two /internal/authorize* endpoints validating genuinely different
// credential types: a real session.Manager-signed token (a service
// token or human session) must not work as an ingest credential, since
// it was never checked against rbacstore.ValidateIngestCredential --
// this endpoint doesn't call session.Manager.Validate at all.
func TestAuthorizeIngestRejectsSessionToken(t *testing.T) {
	h, m := testHandler(t)
	sessionToken, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doAuthorizeIngest(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+sessionToken)
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a session token must not validate as an ingest credential)", rec.Code)
	}
}

func doActiveTenants(t *testing.T, h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/internal/active-tenants", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestActiveTenantsViaServiceToken(t *testing.T) {
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{}, newFakeIngestCredentialValidator(), &fakeTenantLister{ids: []string{"acme", "globex"}})
	token, err := m.IssueServiceToken("search")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}

	rec := doActiveTenants(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body activeTenantsResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.TenantIDs) != 2 || body.TenantIDs[0] != "acme" || body.TenantIDs[1] != "globex" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestActiveTenantsNoCredentialsIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doActiveTenants(t, h, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestActiveTenantsRejectsHumanSession is the regression test for this
// endpoint's whole reason to distinguish token kinds: a human session
// (even a real, validly-signed one) must not be able to list every
// active tenant in the deployment -- only a RoleService credential can.
func TestActiveTenantsRejectsHumanSession(t *testing.T) {
	h, m := testHandler(t)
	sessionToken, err := m.IssueUserSession("acme", "u1", "owner")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	rec := doActiveTenants(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+sessionToken)
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a human session must not satisfy the service-only active-tenants endpoint)", rec.Code)
	}
}

func TestActiveTenantsInvalidTokenIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doActiveTenants(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-real-token")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestActiveTenantsStoreErrorIsInternalError(t *testing.T) {
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{}, newFakeIngestCredentialValidator(), &fakeTenantLister{err: errNotFound})
	token, err := m.IssueServiceToken("search")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doActiveTenants(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
