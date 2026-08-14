// Uses coreos/go-oidc's own oidctest package (a real fake OIDC IdP --
// serves discovery + JWKS, signs real RS256 ID tokens) plus a small
// local /token handler (oidctest doesn't implement the OAuth2 code
// exchange itself, only ID token signing/verification) to exercise the
// FULL login flow -- login redirect, a real signed-and-verified ID
// token round trip, user upsert, tenant/role resolution, and session
// cookie issuance -- with real cryptographic verification, not mocked.
package loginhandler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/sentry/sentry/enterprise/internal/oidc"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/enterprise/internal/session"
)

const testClientID = "sentry-test-client"
const testKeyID = "test-key-1"

// fakeUserStore is an in-memory stand-in for *rbacstore.Store, keyed by
// SSO subject -- enough to drive resolveIdentity's logic without a real
// Postgres.
type fakeUserStore struct {
	usersBySubject map[string]*rbacstore.User
	memberships    map[string][]rbacstore.Membership // by user ID
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{usersBySubject: map[string]*rbacstore.User{}, memberships: map[string][]rbacstore.Membership{}}
}

func (f *fakeUserStore) UpsertUserBySSO(_ context.Context, ssoSubject, email, displayName string) (*rbacstore.User, error) {
	if u, ok := f.usersBySubject[ssoSubject]; ok {
		u.Email, u.DisplayName = email, displayName
		return u, nil
	}
	u := &rbacstore.User{ID: "user-" + ssoSubject, Email: email, DisplayName: displayName, SSOSubject: ssoSubject}
	f.usersBySubject[ssoSubject] = u
	return u, nil
}

func (f *fakeUserStore) ListMembershipsForUser(_ context.Context, userID string) ([]rbacstore.Membership, error) {
	return f.memberships[userID], nil
}

// testIdP bundles a real oidctest.Server (discovery + JWKS) with a local
// /token handler, and knows how to mint a validly-signed ID token for a
// given subject/email -- everything a test needs to drive a real login
// round trip.
type testIdP struct {
	srv         *httptest.Server
	priv        *rsa.PrivateKey
	nextIDToken string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	idp := &testIdP{priv: priv}

	oidcSrv := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{PublicKey: priv.Public(), KeyID: testKeyID, Algorithm: "RS256"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"id_token":     idp.nextIDToken,
			"token_type":   "Bearer",
		})
	})
	mux.Handle("/", oidcSrv)

	idp.srv = httptest.NewServer(mux)
	oidcSrv.SetIssuer(idp.srv.URL)
	t.Cleanup(idp.srv.Close)
	return idp
}

// setNextIDToken configures what /token returns on the next exchange --
// a real RS256-signed JWT, verified for real by oidc.Provider.Exchange.
func (idp *testIdP) setNextIDToken(t *testing.T, subject, email string, emailVerified bool, expiry time.Time) {
	t.Helper()
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": %q, "sub": %q, "email": %q, "email_verified": %v,
		"iat": %d, "exp": %d
	}`, idp.srv.URL, testClientID, subject, email, emailVerified, time.Now().Unix(), expiry.Unix())
	idp.nextIDToken = oidctest.SignIDToken(idp.priv, testKeyID, "RS256", claims)
}

func newTestOIDCProvider(t *testing.T, idp *testIdP) *oidc.Provider {
	t.Helper()
	p, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: idp.srv.URL, ClientID: testClientID, ClientSecret: "secret",
		RedirectURL: "http://sentry-test/auth/oidc/callback",
	})
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return p
}

func newTestSessionManager(t *testing.T) *session.Manager {
	t.Helper()
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	return m
}

func TestHandleLoginRedirectsAndSetsStateCookie(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Fatal("expected a Location header redirecting to the IdP")
	}
	cookies := rec.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatal("expected a non-empty state cookie to be set")
	}
	if !stateCookie.HttpOnly {
		t.Fatal("expected the state cookie to be HttpOnly")
	}
}

// fullLoginFlow drives handleLogin then handleCallback end to end,
// exactly the way a browser + IdP round trip would, and returns the
// final response so callers can assert on it.
func fullLoginFlow(t *testing.T, h *Handler, idp *testIdP) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	var stateCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("no state cookie from /auth/oidc/login")
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+stateCookie.Value+"&code=test-code", nil)
	callbackReq.AddCookie(stateCookie)
	callbackRec := httptest.NewRecorder()
	mux.ServeHTTP(callbackRec, callbackReq)
	return callbackRec
}

func TestFullLoginFlowIssuesSessionForSingleMembership(t *testing.T) {
	idp := newTestIdP(t)
	store := newFakeUserStore()
	store.memberships["user-user-1"] = []rbacstore.Membership{{TenantID: "acme", UserID: "user-user-1", Role: rbacstore.RoleEditor}}
	sessionManager := newTestSessionManager(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), sessionManager, store, "http://web/")

	idp.setNextIDToken(t, "user-1", "person@acme.example", true, time.Now().Add(time.Hour))
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "http://web/" {
		t.Fatalf("Location = %q, want http://web/", loc)
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sentry_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("expected a sentry_session cookie to be set")
	}
	claims, err := sessionManager.Validate(sessionCookie.Value)
	if err != nil {
		t.Fatalf("validating issued session: %v", err)
	}
	if claims.TenantID != "acme" || claims.Role != "editor" || claims.UserID != "user-user-1" {
		t.Fatalf("unexpected session claims: %+v", claims)
	}
}

func TestFullLoginFlowRefusesNoMembership(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), newFakeUserStore(), "http://web/")

	idp.setNextIDToken(t, "user-2", "nobody@acme.example", true, time.Now().Add(time.Hour))
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFullLoginFlowRefusesMultipleMemberships(t *testing.T) {
	idp := newTestIdP(t)
	store := newFakeUserStore()
	store.memberships["user-user-3"] = []rbacstore.Membership{
		{TenantID: "acme", UserID: "user-user-3", Role: rbacstore.RoleViewer},
		{TenantID: "globex", UserID: "user-user-3", Role: rbacstore.RoleAdmin},
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), store, "http://web/")

	idp.setNextIDToken(t, "user-3", "multi@example.com", true, time.Now().Add(time.Hour))
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=wrong&code=test-code", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "correct"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCallbackRejectsMissingStateCookie(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=whatever&code=test-code", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCallbackRejectsExpiredIDToken(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), newTestSessionManager(t), newFakeUserStore(), "http://web/")

	idp.setNextIDToken(t, "user-4", "person@example.com", true, time.Now().Add(-time.Hour)) // already expired
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRoutesNoOpWhenOIDCNotConfigured(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no routes should be registered when oidc is nil)", rec.Code)
	}
}

// TestRegisterRoutesNoOpWithTypedNilProviderVariable is the regression
// test for Go's typed-nil-interface trap: enterprise-auth's main.go
// holds a `var provider *oidc.Provider` that stays nil when OIDC isn't
// configured, then passes that *variable* (not a nil literal) into New.
// If New ever goes back to assigning that pointer straight into the
// oidcProvider interface field, this test starts failing -- the
// interface would become non-nil (type=*oidc.Provider, value=nil) even
// though the variable itself is nil, and RegisterRoutes' `h.oidc == nil`
// check would stop working. TestRegisterRoutesNoOpWhenOIDCNotConfigured
// above doesn't catch this: passing a nil literal directly never hits
// the trap, only passing a nil-valued typed variable does.
func TestRegisterRoutesNoOpWithTypedNilProviderVariable(t *testing.T) {
	var provider *oidc.Provider // stays nil -- exactly main.go's shape when OIDC_ISSUER_URL is unset
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), provider, newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a typed-nil *oidc.Provider must still result in oidc routes being disabled)", rec.Code)
	}
}
