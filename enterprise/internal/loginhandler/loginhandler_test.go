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
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/cairnobs/cairnobs/enterprise/internal/oidc"
	"github.com/cairnobs/cairnobs/enterprise/internal/rbacstore"
	"github.com/cairnobs/cairnobs/enterprise/internal/session"
)

const testClientID = "cairnobs-test-client"
const testKeyID = "test-key-1"

// fakeUserStore is an in-memory stand-in for *rbacstore.Store, keyed by
// SSO subject -- enough to drive resolveIdentity's logic without a real
// Postgres.
type fakeUserStore struct {
	usersBySubject     map[string]*rbacstore.User
	memberships        map[string][]rbacstore.Membership // by user ID
	tenantDisplayNames map[string]string                 // by tenant ID, defaults to the ID itself if unset
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		usersBySubject:     map[string]*rbacstore.User{},
		memberships:        map[string][]rbacstore.Membership{},
		tenantDisplayNames: map[string]string{},
	}
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

func (f *fakeUserStore) ListMembershipsWithTenantForUser(_ context.Context, userID string) ([]rbacstore.MembershipWithTenant, error) {
	memberships := f.memberships[userID]
	out := make([]rbacstore.MembershipWithTenant, 0, len(memberships))
	for _, m := range memberships {
		displayName := f.tenantDisplayNames[m.TenantID]
		if displayName == "" {
			displayName = m.TenantID
		}
		out = append(out, rbacstore.MembershipWithTenant{TenantID: m.TenantID, TenantDisplayName: displayName, Role: m.Role})
	}
	return out, nil
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
		RedirectURL: "http://cairnobs-test/auth/oidc/callback",
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
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
		if c.Name == oidcStateCookieName {
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
		if c.Name == oidcStateCookieName {
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, sessionManager, store, "http://web/", "http://web/select-tenant")

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
		if c.Name == "cairnobs_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("expected a cairnobs_session cookie to be set")
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")

	idp.setNextIDToken(t, "user-2", "nobody@acme.example", true, time.Now().Add(time.Hour))
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestFullLoginFlowStartsTenantSelectionForMultipleMemberships is the
// regression test for the real tenant-picker protocol (see this
// package's doc comment): an identity with more than one
// tenant_memberships row must get a pending-login cookie and a redirect
// to selectTenantRedirectURL, not a flat refusal and not a session
// cookie for either tenant guessed at.
func TestFullLoginFlowStartsTenantSelectionForMultipleMemberships(t *testing.T) {
	idp := newTestIdP(t)
	store := newFakeUserStore()
	store.memberships["user-user-3"] = []rbacstore.Membership{
		{TenantID: "acme", UserID: "user-user-3", Role: rbacstore.RoleViewer},
		{TenantID: "globex", UserID: "user-user-3", Role: rbacstore.RoleAdmin},
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), store, "http://web/", "http://web/select-tenant")

	idp.setNextIDToken(t, "user-3", "multi@example.com", true, time.Now().Add(time.Hour))
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "http://web/select-tenant" {
		t.Fatalf("Location = %q, want http://web/select-tenant", loc)
	}
	var pendingCookie, sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case pendingLoginCookieName:
			pendingCookie = c
		case "cairnobs_session":
			sessionCookie = c
		}
	}
	if pendingCookie == nil || pendingCookie.Value == "" {
		t.Fatal("expected a non-empty pending-login cookie")
	}
	if sessionCookie != nil {
		t.Fatal("must not issue a real session cookie before a tenant is actually chosen")
	}
}

// startTenantSelection drives a full OIDC login for an identity that
// resolves to more than one membership, returning the mux (so callers
// can drive GET /auth/memberships and POST /auth/select-tenant
// afterward, exactly like a picker page would) and the pending-login
// cookie the login flow set.
func startTenantSelection(t *testing.T, h *Handler, idp *testIdP) (*http.ServeMux, *http.Cookie) {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	var stateCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == oidcStateCookieName {
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
	if callbackRec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", callbackRec.Code, callbackRec.Body.String())
	}
	var pendingCookie *http.Cookie
	for _, c := range callbackRec.Result().Cookies() {
		if c.Name == pendingLoginCookieName {
			pendingCookie = c
		}
	}
	if pendingCookie == nil {
		t.Fatal("expected a pending-login cookie")
	}
	return mux, pendingCookie
}

func newMultiMembershipStore() *fakeUserStore {
	store := newFakeUserStore()
	store.memberships["user-user-multi"] = []rbacstore.Membership{
		{TenantID: "acme", UserID: "user-user-multi", Role: rbacstore.RoleViewer},
		{TenantID: "globex", UserID: "user-user-multi", Role: rbacstore.RoleAdmin},
	}
	store.tenantDisplayNames["acme"] = "Acme Corp"
	store.tenantDisplayNames["globex"] = "Globex Corporation"
	return store
}

func TestListMembershipsReturnsTenantOptions(t *testing.T) {
	idp := newTestIdP(t)
	store := newMultiMembershipStore()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), store, "http://web/", "http://web/select-tenant")
	idp.setNextIDToken(t, "user-multi", "multi@example.com", true, time.Now().Add(time.Hour))
	mux, pendingCookie := startTenantSelection(t, h, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/memberships", nil)
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var options []membershipOption
	if err := json.Unmarshal(rec.Body.Bytes(), &options); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("got %d options, want 2: %+v", len(options), options)
	}
	byTenant := map[string]membershipOption{}
	for _, o := range options {
		byTenant[o.TenantID] = o
	}
	if byTenant["acme"].TenantDisplayName != "Acme Corp" || byTenant["acme"].Role != "viewer" {
		t.Fatalf("unexpected acme option: %+v", byTenant["acme"])
	}
	if byTenant["globex"].TenantDisplayName != "Globex Corporation" || byTenant["globex"].Role != "admin" {
		t.Fatalf("unexpected globex option: %+v", byTenant["globex"])
	}
}

func TestListMembershipsRejectsMissingPendingCookie(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/memberships", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSelectTenantIssuesSessionForChosenTenant(t *testing.T) {
	idp := newTestIdP(t)
	store := newMultiMembershipStore()
	sessionManager := newTestSessionManager(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, sessionManager, store, "http://web/", "http://web/select-tenant")
	idp.setNextIDToken(t, "user-multi", "multi@example.com", true, time.Now().Add(time.Hour))
	mux, pendingCookie := startTenantSelection(t, h, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/select-tenant", strings.NewReader(`{"tenant_id": "globex"}`))
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var sessionCookie, clearedPendingCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "cairnobs_session":
			sessionCookie = c
		case pendingLoginCookieName:
			clearedPendingCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("expected a cairnobs_session cookie to be set")
	}
	if clearedPendingCookie == nil || clearedPendingCookie.MaxAge >= 0 {
		t.Fatalf("expected the pending-login cookie to be cleared (MaxAge < 0), got %+v", clearedPendingCookie)
	}
	claims, err := sessionManager.Validate(sessionCookie.Value)
	if err != nil {
		t.Fatalf("validating issued session: %v", err)
	}
	if claims.TenantID != "globex" || claims.Role != "admin" || claims.UserID != "user-user-multi" {
		t.Fatalf("unexpected session claims: %+v", claims)
	}

	var body selectTenantResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.RedirectURL != "http://web/" {
		t.Fatalf("RedirectURL = %q, want http://web/", body.RedirectURL)
	}
}

// TestSelectTenantRejectsTenantOutsideMembership is the regression test
// for handleSelectTenant re-deriving role server-side rather than
// trusting the request: a client claiming a tenant_id the pending
// identity doesn't actually belong to must be refused, not silently
// granted whatever role happened to be in the request.
func TestSelectTenantRejectsTenantOutsideMembership(t *testing.T) {
	idp := newTestIdP(t)
	store := newMultiMembershipStore()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), store, "http://web/", "http://web/select-tenant")
	idp.setNextIDToken(t, "user-multi", "multi@example.com", true, time.Now().Add(time.Hour))
	mux, pendingCookie := startTenantSelection(t, h, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/select-tenant", strings.NewReader(`{"tenant_id": "not-a-real-membership"}`))
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "cairnobs_session" {
			t.Fatal("must not issue a session cookie for a tenant outside the identity's memberships")
		}
	}
}

func TestSelectTenantRejectsMissingBody(t *testing.T) {
	idp := newTestIdP(t)
	store := newMultiMembershipStore()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), store, "http://web/", "http://web/select-tenant")
	idp.setNextIDToken(t, "user-multi", "multi@example.com", true, time.Now().Add(time.Hour))
	mux, pendingCookie := startTenantSelection(t, h, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/select-tenant", strings.NewReader(`{}`))
	req.AddCookie(pendingCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSelectTenantRejectsMissingPendingCookie(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/select-tenant", strings.NewReader(`{"tenant_id": "acme"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestSelectTenantRejectsSessionCookieAsPendingCookie is the regression
// test for PendingLoginClaims being a distinct type from Claims: a real
// session token must not work as a pending-login cookie, even though
// both are signed by the same key.
func TestSelectTenantRejectsSessionCookieAsPendingCookie(t *testing.T) {
	idp := newTestIdP(t)
	store := newFakeUserStore()
	store.memberships["user-user-1"] = []rbacstore.Membership{{TenantID: "acme", UserID: "user-user-1", Role: rbacstore.RoleEditor}}
	sessionManager := newTestSessionManager(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, sessionManager, store, "http://web/", "http://web/select-tenant")

	realSessionToken, err := sessionManager.IssueUserSession("acme", "user-user-1", "editor")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/auth/select-tenant", strings.NewReader(`{"tenant_id": "acme"}`))
	req.AddCookie(&http.Cookie{Name: pendingLoginCookieName, Value: realSessionToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (a real session token must not validate as a pending login)", rec.Code)
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=wrong&code=test-code", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: "correct"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCallbackRejectsMissingStateCookie(t *testing.T) {
	idp := newTestIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), newTestOIDCProvider(t, idp), nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")

	idp.setNextIDToken(t, "user-4", "person@example.com", true, time.Now().Add(-time.Hour)) // already expired
	rec := fullLoginFlow(t, h, idp)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRoutesNoOpWhenOIDCNotConfigured(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), provider, nil, newTestSessionManager(t), newFakeUserStore(), "http://web/", "http://web/select-tenant")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a typed-nil *oidc.Provider must still result in oidc routes being disabled)", rec.Code)
	}
}
