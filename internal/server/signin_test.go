package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/fakeinbuxa"
)

const testRedirect = "https://beacon.test/auth/inbuxa/callback"

func signinServer(t *testing.T, edit ...func(*ProviderConfig)) (*Server, *fakeinbuxa.Server) {
	t.Helper()
	fake := fakeinbuxa.New("cairn-obs", testRedirect)
	var fs *httptest.Server
	fs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fake.Handler(fs.URL).ServeHTTP(w, r) }))
	t.Cleanup(fs.Close)
	pc := ProviderConfig{ID: "inbuxa", Type: "inbuxa", Issuer: fs.URL, ClientID: "cairn-obs", RedirectURL: testRedirect}
	for _, e := range edit {
		e(&pc)
	}
	if err := pc.check(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(&Config{Listen: "127.0.0.1:0", StaleAfter: time.Minute, Signin: SigninConfig{Providers: []ProviderConfig{pc}}})
	if err != nil {
		t.Fatal(err)
	}
	return srv, fake
}

// signIn runs the whole flow as a browser would and returns obs-beacond's final
// redirect and the session cookie (nil when none was set).
func signIn(t *testing.T, srv *Server, fake *fakeinbuxa.Server, account string) (string, *http.Cookie) {
	t.Helper()
	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/inbuxa/login?return=/containers", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	oauth := rec.Result().Cookies()[0]
	if oauth.SameSite != http.SameSiteLaxMode || !oauth.HttpOnly {
		t.Fatalf("state cookie: %+v", oauth)
	}
	authorize := rec.Header().Get("Location")
	if !strings.Contains(authorize, "code_challenge_method=S256") || strings.Contains(authorize, "code_verifier") {
		t.Fatalf("authorize URL: %s", authorize)
	}
	back, err := fake.Approve(authorize, account)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(back)
	req := httptest.NewRequest("GET", u.Path+"?"+u.RawQuery, nil)
	req.AddCookie(oauth)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return rec.Header().Get("Location"), c
		}
	}
	return rec.Header().Get("Location"), nil
}

func get(h http.Handler, path string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSigninAdminGetsIn(t *testing.T) {
	srv, fake := signinServer(t)
	to, cookie := signIn(t, srv, fake, "admin")
	if to != "/containers" || cookie == nil {
		t.Fatalf("redirect %q, cookie %v", to, cookie)
	}
	rec := get(srv.Handler(), "/api/v1/session", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"user":"Pat Admin"`) || !strings.Contains(rec.Body.String(), `"role":"admin"`) {
		t.Fatalf("session: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("session leaks a token to the browser: %s", rec.Body)
	}
}

func TestSigninTenantAdminRefused(t *testing.T) {
	srv, fake := signinServer(t)
	to, cookie := signIn(t, srv, fake, "tenant")
	if to != "/?signin=refused" || cookie != nil {
		t.Fatalf("tenant admin: redirect %q, cookie %v", to, cookie)
	}
	if n := len(srv.sessions.m); n != 0 {
		t.Fatalf("%d sessions kept for a refused account", n)
	}
}

// operator_permissions short of an admin's make an operator.
func TestSigninOperatorPermissions(t *testing.T) {
	srv, fake := signinServer(t, func(c *ProviderConfig) { c.OperatorPermissions = []string{"sysTenantGet"} })
	_, cookie := signIn(t, srv, fake, "tenant")
	if cookie == nil || !strings.Contains(get(srv.Handler(), "/api/v1/session", cookie).Body.String(), `"role":"operator"`) {
		t.Fatalf("tenant with operator permissions: cookie %v", cookie)
	}
	if _, cookie = signIn(t, srv, fake, "admin"); !strings.Contains(get(srv.Handler(), "/api/v1/session", cookie).Body.String(), `"role":"admin"`) {
		t.Fatal("an admin with operator permissions too isn't an admin")
	}
}

func TestSigninStateMustMatch(t *testing.T) {
	srv, fake := signinServer(t)
	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/inbuxa/login", nil))
	back, _ := fake.Approve(rec.Header().Get("Location"), "admin")
	u, _ := url.Parse(back)
	// Same callback, but from a browser without the state cookie (a forged link).
	rec = get(h, u.Path+"?"+u.RawQuery, nil)
	if rec.Header().Get("Location") != "/?signin=expired" {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSigninReturnStaysOnBeacon(t *testing.T) {
	for _, bad := range []string{"https://evil.test/", "//evil.test/", "/\\evil.test", "/auth/inbuxa/login"} {
		if got := safeReturn(bad); got != "/" {
			t.Errorf("safeReturn(%q) = %q", bad, got)
		}
	}
	if got := safeReturn("/hosts/host2"); got != "/hosts/host2" {
		t.Errorf("good path rewritten to %q", got)
	}
}

func TestRecheckEndsRemovedAdmins(t *testing.T) {
	srv, fake := signinServer(t)
	_, cookie := signIn(t, srv, fake, "admin")
	h := srv.Handler()
	srv.providers["inbuxa"].(*inbuxa).cfg.Recheck = time.Nanosecond // check on every request

	if rec := get(h, "/api/v1/overview", cookie); rec.Code != http.StatusOK {
		t.Fatalf("before: %d", rec.Code)
	}
	// inbuxa down: the session survives inside the grace period.
	fake.SetDown(true)
	if rec := get(h, "/api/v1/overview", cookie); rec.Code != http.StatusOK {
		t.Fatalf("inbuxa down: %d (should survive the grace period)", rec.Code)
	}
	fake.SetDown(false)
	// The admin role is taken away in inbuxa: the next check ends the session.
	fake.SetPermissions("admin", []string{"authenticate"})
	if rec := get(h, "/api/v1/overview", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after role removal: %d", rec.Code)
	}
	if rec := get(h, "/api/v1/overview", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatal("session came back")
	}
}

func TestRecheckGraceRunsOut(t *testing.T) {
	srv, fake := signinServer(t)
	_, cookie := signIn(t, srv, fake, "admin")
	p := srv.providers["inbuxa"].(*inbuxa)
	p.cfg.Recheck, p.cfg.Grace = time.Nanosecond, time.Nanosecond
	fake.SetDown(true)
	if rec := get(srv.Handler(), "/api/v1/overview", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("grace over, inbuxa still down: %d", rec.Code)
	}
}

func TestSigninOptionsListProviders(t *testing.T) {
	srv, _ := signinServer(t)
	rec := get(srv.Handler(), "/api/v1/session", nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `{"id":"inbuxa","kind":"redirect","label":"Sign in with inbuxa","note":"Only inbuxa System Administrators get in that way; other accounts are refused."}`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestProviderConfigChecks(t *testing.T) {
	for _, pc := range []ProviderConfig{
		{ID: "inbuxa", Type: "inbuxa", Issuer: "http://mail.example.org", ClientID: "cairn-obs", RedirectURL: "https://b/auth/inbuxa/callback"}, // plain http off loopback
		{ID: "inbuxa", Type: "inbuxa", Issuer: "https://mail.example.org", ClientID: "cairn-obs", RedirectURL: "https://b/callback"},            // wrong callback path
		{ID: "okta", Type: "oidc"}, // planned, not built
		{ID: "x", Type: "nonsense"},
		{ID: "a/b", Type: "inbuxa"},
	} {
		if err := pc.check(); err == nil {
			t.Errorf("accepted %+v", pc)
		}
	}
}

func TestSigninAsksForRefreshAndFallsBackForName(t *testing.T) {
	srv, fake := signinServer(t)
	fake.BareUserinfo = true
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/auth/inbuxa/login", nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "scope=openid+offline_access") {
		t.Fatalf("scope: %s", loc)
	}
	_, cookie := signIn(t, srv, fake, "admin")
	body := get(srv.Handler(), "/api/v1/session", cookie).Body.String()
	if !strings.Contains(body, `"user":"pat@example.org"`) {
		t.Fatalf("name fallback: %s", body)
	}
}
