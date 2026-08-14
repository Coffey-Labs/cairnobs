// Mirrors loginhandler_test.go's OIDC approach: exercise the full SAML
// login flow against a real fake IdP rather than mocking anything.
// crewjam/saml ships samlidp, a genuine SAML identity provider (real XML
// signing, real assertion construction) meant for exactly this kind of
// testing. To avoid driving its HTML login form, a valid saml.Session is
// seeded directly into the IdP's session store and presented via the
// `session` cookie GetSession already accepts -- confirmed by reading
// samlidp's own GetSession implementation, the same "skip the UI, keep
// the crypto real" shortcut oidctest gives the OIDC tests above.
package loginhandler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlidp"

	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	samlpkg "github.com/sentry/sentry/enterprise/internal/saml"
)

const (
	testSAMLEntityID = "https://sentry-test.example.com/saml/metadata"
	testSAMLACSURL   = "https://sentry-test.example.com/auth/saml/acs"
)

// testSAMLIdP bundles a real samlidp.Server with the SP key/cert it was
// registered against, enough to drive a full SP-initiated login.
type testSAMLIdP struct {
	server *samlidp.Server
	store  *samlidp.MemoryStore
	spKey  *rsa.PrivateKey
	spCert *x509.Certificate
}

func genSelfSignedCert(t *testing.T, commonName string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generating serial number: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	return key, cert
}

// newTestSAMLIdP starts a real samlidp.Server and registers Sentry's SP
// metadata with it directly via the IdP's own PUT /services/{id}
// endpoint -- the same mechanism a real IdP admin uses, not a shortcut
// that reaches into samlidp's unexported state.
func newTestSAMLIdP(t *testing.T) *testSAMLIdP {
	t.Helper()
	idpKey, idpCert := genSelfSignedCert(t, "sentry-test-idp")
	spKey, spCert := genSelfSignedCert(t, "sentry-test-sp")

	store := &samlidp.MemoryStore{}
	idpServer, err := samlidp.New(samlidp.Options{
		Key:         idpKey,
		Certificate: idpCert,
		Store:       store,
		URL:         url.URL{Scheme: "http", Host: "idp.example.com"},
	})
	if err != nil {
		t.Fatalf("samlidp.New: %v", err)
	}

	entityIDURL, err := url.Parse(testSAMLEntityID)
	if err != nil {
		t.Fatalf("parsing test entity id: %v", err)
	}
	acsURL, err := url.Parse(testSAMLACSURL)
	if err != nil {
		t.Fatalf("parsing test acs url: %v", err)
	}
	// A throwaway saml.ServiceProvider built from the same
	// EntityID/ACSURL/cert samlpkg.New below uses -- Metadata() is a
	// pure function of those exported fields, so this stays consistent
	// with the real samlpkg.ServiceProvider without needing access to
	// its unexported inner sp field.
	spForRegistration := saml.ServiceProvider{
		Key:         spKey,
		Certificate: spCert,
		MetadataURL: *entityIDURL,
		AcsURL:      *acsURL,
	}
	spMetadataXML, err := xml.Marshal(spForRegistration.Metadata())
	if err != nil {
		t.Fatalf("marshaling sp metadata: %v", err)
	}
	putReq := httptest.NewRequest(http.MethodPut, "/services/sentry-test-sp", strings.NewReader(string(spMetadataXML)))
	putRec := httptest.NewRecorder()
	idpServer.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusNoContent {
		t.Fatalf("registering sp metadata with fake idp: status = %d, body = %s", putRec.Code, putRec.Body.String())
	}

	return &testSAMLIdP{server: idpServer, store: store, spKey: spKey, spCert: spCert}
}

// serviceProvider builds the samlpkg.ServiceProvider loginhandler uses,
// trusting idp's metadata.
func (idp *testSAMLIdP) serviceProvider(t *testing.T) *samlpkg.ServiceProvider {
	t.Helper()
	sp, err := samlpkg.New(samlpkg.Config{
		EntityID:    testSAMLEntityID,
		ACSURL:      testSAMLACSURL,
		IDPMetadata: idp.server.IDP.Metadata(),
		Certificate: &tls.Certificate{
			Certificate: [][]byte{idp.spCert.Raw},
			PrivateKey:  idp.spKey,
		},
	})
	if err != nil {
		t.Fatalf("samlpkg.New: %v", err)
	}
	return sp
}

// seedSession pre-authenticates a user directly in the fake IdP's store,
// bypassing its login-form HTML entirely. Confirmed viable by reading
// samlidp's GetSession: a valid, non-expired saml.Session at
// /sessions/<id> plus a matching `session` cookie is exactly what a real
// login-form POST would have produced -- this is the IdP's own supported
// shortcut, not an abuse of internals.
func (idp *testSAMLIdP) seedSession(t *testing.T, nameID, email string) *http.Cookie {
	t.Helper()
	sessionID := fmt.Sprintf("test-session-%d", time.Now().UnixNano())
	session := &saml.Session{
		ID:         sessionID,
		NameID:     nameID,
		CreateTime: saml.TimeNow(),
		ExpireTime: saml.TimeNow().Add(time.Hour),
		Index:      sessionID,
		UserEmail:  email,
	}
	if err := idp.store.Put(fmt.Sprintf("/sessions/%s", sessionID), session); err != nil {
		t.Fatalf("seeding idp session: %v", err)
	}
	return &http.Cookie{Name: "session", Value: sessionID}
}

var samlResponseFieldRe = regexp.MustCompile(`name="(SAMLResponse|RelayState)" value="([^"]*)"`)

// extractSAMLResponseForm pulls the hidden form fields out of the IdP's
// auto-submitting HTML response -- what a real browser's inline <script>
// reads before POSTing to the SP's ACS endpoint.
func extractSAMLResponseForm(t *testing.T, body string) (samlResponse, relayState string) {
	t.Helper()
	for _, m := range samlResponseFieldRe.FindAllStringSubmatch(body, -1) {
		switch m[1] {
		case "SAMLResponse":
			samlResponse = html.UnescapeString(m[2])
		case "RelayState":
			relayState = html.UnescapeString(m[2])
		}
	}
	if samlResponse == "" {
		t.Fatalf("no SAMLResponse field found in idp response html: %s", body)
	}
	return samlResponse, relayState
}

// fullSAMLLoginFlow drives handleSAMLLogin, the fake IdP's /sso, and
// handleSAMLACS end to end, exactly the way a browser + IdP round trip
// would, and returns the final response so callers can assert on it.
func fullSAMLLoginFlow(t *testing.T, h *Handler, idp *testSAMLIdP, nameID, email string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("GET /auth/saml/login: status = %d, body = %s", loginRec.Code, loginRec.Body.String())
	}
	redirectURL := loginRec.Header().Get("Location")
	var requestCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == samlRequestCookieName {
			requestCookie = c
		}
	}
	if requestCookie == nil {
		t.Fatal("no saml request cookie from /auth/saml/login")
	}

	ssoReq := httptest.NewRequest(http.MethodGet, redirectURL, nil)
	ssoReq.AddCookie(idp.seedSession(t, nameID, email))
	ssoRec := httptest.NewRecorder()
	idp.server.ServeHTTP(ssoRec, ssoReq)
	if ssoRec.Code != http.StatusOK {
		t.Fatalf("idp GET /sso: status = %d, body = %s", ssoRec.Code, ssoRec.Body.String())
	}
	samlResponse, relayState := extractSAMLResponseForm(t, ssoRec.Body.String())

	form := url.Values{"SAMLResponse": {samlResponse}, "RelayState": {relayState}}
	acsReq := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader(form.Encode()))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acsReq.AddCookie(requestCookie)
	acsRec := httptest.NewRecorder()
	mux.ServeHTTP(acsRec, acsReq)
	return acsRec
}

func TestHandleSAMLLoginRedirectsAndSetsRequestCookie(t *testing.T) {
	idp := newTestSAMLIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Fatal("expected a Location header redirecting to the IdP")
	}
	var requestCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == samlRequestCookieName {
			requestCookie = c
		}
	}
	if requestCookie == nil || requestCookie.Value == "" {
		t.Fatal("expected a non-empty saml request cookie to be set")
	}
	if !requestCookie.HttpOnly {
		t.Fatal("expected the saml request cookie to be HttpOnly")
	}
	if requestCookie.SameSite != http.SameSiteNoneMode {
		t.Fatalf("SameSite = %v, want SameSiteNoneMode -- the acs POST is cross-site from the idp's origin", requestCookie.SameSite)
	}
}

func TestFullSAMLLoginFlowIssuesSessionForSingleMembership(t *testing.T) {
	idp := newTestSAMLIdP(t)
	store := newFakeUserStore()
	store.memberships["user-saml-user-1"] = []rbacstore.Membership{{TenantID: "acme", UserID: "user-saml-user-1", Role: rbacstore.RoleEditor}}
	sessionManager := newTestSessionManager(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), sessionManager, store, "http://web/")

	rec := fullSAMLLoginFlow(t, h, idp, "saml-user-1", "person@acme.example")

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
	if claims.TenantID != "acme" || claims.Role != "editor" || claims.UserID != "user-saml-user-1" {
		t.Fatalf("unexpected session claims: %+v", claims)
	}
}

func TestFullSAMLLoginFlowRefusesNoMembership(t *testing.T) {
	idp := newTestSAMLIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), newFakeUserStore(), "http://web/")

	rec := fullSAMLLoginFlow(t, h, idp, "saml-user-2", "nobody@acme.example")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFullSAMLLoginFlowRefusesMultipleMemberships(t *testing.T) {
	idp := newTestSAMLIdP(t)
	store := newFakeUserStore()
	store.memberships["user-saml-user-3"] = []rbacstore.Membership{
		{TenantID: "acme", UserID: "user-saml-user-3", Role: rbacstore.RoleViewer},
		{TenantID: "globex", UserID: "user-saml-user-3", Role: rbacstore.RoleAdmin},
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), store, "http://web/")

	rec := fullSAMLLoginFlow(t, h, idp, "saml-user-3", "multi@example.com")

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFullSAMLLoginFlowRefusesMissingEmail(t *testing.T) {
	idp := newTestSAMLIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), newFakeUserStore(), "http://web/")

	rec := fullSAMLLoginFlow(t, h, idp, "saml-user-4", "") // no email attribute in the assertion

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSAMLACSRejectsMissingRequestCookie(t *testing.T) {
	idp := newTestSAMLIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// No prior GET /auth/saml/login, so no sentry_saml_request cookie --
	// simulates an attacker POSTing a captured/forged response directly
	// at the ACS endpoint with no matching request state.
	form := url.Values{"SAMLResponse": {"irrelevant"}, "RelayState": {""}}
	req := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSAMLACSRejectsWrongRequestID(t *testing.T) {
	idp := newTestSAMLIdP(t)
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idp.serviceProvider(t), newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	redirectURL := loginRec.Header().Get("Location")

	ssoReq := httptest.NewRequest(http.MethodGet, redirectURL, nil)
	ssoReq.AddCookie(idp.seedSession(t, "saml-user-5", "person@acme.example"))
	ssoRec := httptest.NewRecorder()
	idp.server.ServeHTTP(ssoRec, ssoReq)
	samlResponse, relayState := extractSAMLResponseForm(t, ssoRec.Body.String())

	// Present the genuine, correctly-signed response but with a
	// tampered request cookie -- the InResponseTo check must still
	// reject it. This is SAML's replay/unsolicited-response defense,
	// the mechanism samlRequestCookieName exists for.
	form := url.Values{"SAMLResponse": {samlResponse}, "RelayState": {relayState}}
	acsReq := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader(form.Encode()))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acsReq.AddCookie(&http.Cookie{Name: samlRequestCookieName, Value: "some-other-request-id"})
	acsRec := httptest.NewRecorder()
	mux.ServeHTTP(acsRec, acsReq)

	if acsRec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", acsRec.Code, acsRec.Body.String())
	}
}

func TestRegisterRoutesNoOpWhenSAMLNotConfigured(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no routes should be registered when saml is nil)", rec.Code)
	}
}

// TestRegisterRoutesNoOpWithTypedNilSAMLProviderVariable is SAML's
// equivalent of TestRegisterRoutesNoOpWithTypedNilProviderVariable in
// loginhandler_test.go -- see that test's doc comment for the Go
// typed-nil-interface trap this guards against.
func TestRegisterRoutesNoOpWithTypedNilSAMLProviderVariable(t *testing.T) {
	var provider *samlpkg.ServiceProvider // stays nil -- main.go's shape when SAML_IDP_METADATA_URL is unset
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, provider, newTestSessionManager(t), newFakeUserStore(), "http://web/")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a typed-nil *samlpkg.ServiceProvider must still result in saml routes being disabled)", rec.Code)
	}
}
