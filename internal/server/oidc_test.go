package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOIDC is a provider for the tests: discovery, JWKS, a token endpoint
// (code and refresh) that signs ID tokens, and userinfo.
type fakeOIDC struct {
	t      *testing.T
	srv    *httptest.Server
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	forge  *rsa.PrivateKey // signs with this instead, as a forger would
	kid    string          // the RSA key's id; changing it is a key rotation
	useEC  bool

	mu        sync.Mutex
	claims    map[string]any // the next ID token's claims, on top of iss/aud/sub/exp/iat/nonce
	userinfo  map[string]any // nil: no userinfo endpoint
	codes     map[string]url.Values
	noRefresh bool
	refreshOK bool // false: refresh is refused (400)
	down      bool
	noIDOnRef bool // refresh answers without a new ID token
	tamper    func(map[string]any)
	basic     string // the client authentication the token endpoint last saw
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	f := &fakeOIDC{t: t, kid: "k1", codes: map[string]url.Values{}, refreshOK: true,
		claims: map[string]any{"name": "Ada", "email": "ada@example.org", "email_verified": true, "groups": []string{"obs-admins"}}}
	f.rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	f.ecKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeOIDC) sign(claims map[string]any) string {
	alg, kid := "RS256", f.kid
	if f.useEC {
		alg, kid = "ES256", "ec1"
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	signed := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	if f.useEC {
		r, s, _ := ecdsa.Sign(rand.Reader, f.ecKey, sum[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	} else {
		key := f.rsaKey
		if f.forge != nil {
			key = f.forge
		}
		sig, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	}
	return signed + "." + enc(sig)
}

func (f *fakeOIDC) idToken(nonce string) string {
	c := map[string]any{"iss": f.srv.URL, "aud": "obs", "sub": "u-1", "exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	for k, v := range f.claims {
		c[k] = v
	}
	if f.tamper != nil {
		f.tamper(c)
	}
	return f.sign(c)
}

func (f *fakeOIDC) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	j := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		d := map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "scopes_supported": []string{"openid", "email", "profile", "offline_access"}}
		if f.userinfo != nil {
			d["userinfo_endpoint"] = f.srv.URL + "/userinfo"
		}
		j(d)
	case "/jwks":
		pub := f.rsaKey.PublicKey
		ec := f.ecKey.PublicKey
		ecb, _ := ec.Bytes()
		j(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": f.kid, "use": "sig", "n": enc(pub.N.Bytes()), "e": enc(big.NewInt(int64(pub.E)).Bytes())},
			{"kty": "EC", "kid": "ec1", "crv": "P-256", "x": enc(ecb[1:33]), "y": enc(ecb[33:])},
		}})
	case "/token":
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		f.basic = user + ":" + pass
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			a, ok := f.codes[r.Form.Get("code")]
			delete(f.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || enc(sum[:]) != a.Get("code_challenge") || r.Form.Get("redirect_uri") != a.Get("redirect_uri") {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			out := map[string]any{"access_token": "at-1", "token_type": "Bearer", "expires_in": 300, "id_token": f.idToken(a.Get("nonce"))}
			if !f.noRefresh {
				out["refresh_token"] = "rt-1"
			}
			j(out)
		case "refresh_token":
			if !f.refreshOK || r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			out := map[string]any{"access_token": "at-2", "token_type": "Bearer", "expires_in": 300}
			if !f.noIDOnRef {
				out["id_token"] = f.idToken("")
			}
			j(out)
		}
	case "/userinfo":
		if f.userinfo == nil || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		j(f.userinfo)
	default:
		http.NotFound(w, r)
	}
}

// approve plays the provider's sign-in page: it takes the authorization
// request and sends back the callback address with a code.
func (f *fakeOIDC) approve(authorize string) string {
	u, _ := url.Parse(authorize)
	q := u.Query()
	f.mu.Lock()
	code := newToken()
	f.codes[code] = q
	f.mu.Unlock()
	return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

const oidcRedirect = "https://obs.test/auth/sso/callback"

func oidcConfig(f *fakeOIDC) ProviderConfig {
	return ProviderConfig{ID: "sso", Type: "oidc", Issuer: f.srv.URL, ClientID: "obs", RedirectURL: oidcRedirect,
		AdminGroups: []string{"obs-admins"}, OperatorGroups: []string{"obs-ops"}, ViewerGroups: []string{"obs-viewers"}}
}

func oidcServer(t *testing.T, f *fakeOIDC, edit func(*ProviderConfig)) *Server {
	t.Helper()
	pc := oidcConfig(f)
	if edit != nil {
		edit(&pc)
	}
	if err := pc.check(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(&Config{Listen: "127.0.0.1:0", StaleAfter: time.Minute, Signin: SigninConfig{Providers: []ProviderConfig{pc}}})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// oidcSignIn runs the browser's part and returns the final redirect and
// the session cookie, if one was set.
func oidcSignIn(t *testing.T, srv *Server, f *fakeOIDC) (string, *http.Cookie) {
	t.Helper()
	h := srv.Handler()
	rec := get(h, "/auth/sso/login?return=/hosts", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	authorize := rec.Header().Get("Location")
	for _, want := range []string{"code_challenge_method=S256", "nonce=", "scope=openid+email+profile+offline_access"} {
		if !strings.Contains(authorize, want) {
			t.Fatalf("authorize URL lacks %s: %s", want, authorize)
		}
	}
	oauth := rec.Result().Cookies()[0]
	u, _ := url.Parse(f.approve(authorize))
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

func TestOIDCGroupsDecideTheRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups any
		want   string // role, or "" for refused
	}{
		{"admin", []string{"staff", "obs-admins"}, "admin"},
		{"viewer", []string{"obs-viewers"}, "viewer"},
		{"both", []string{"obs-viewers", "obs-admins"}, "admin"},
		{"operator", []string{"obs-ops"}, "operator"},
		{"operator over viewer", []string{"obs-viewers", "obs-ops"}, "operator"},
		{"admin over operator", []string{"obs-ops", "obs-admins"}, "admin"},
		{"one string", "obs-admins", "admin"},
		{"neither", []string{"staff"}, ""},
		{"no claim", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOIDC(t)
			f.claims["groups"] = tc.groups
			if tc.groups == nil {
				delete(f.claims, "groups")
			}
			srv := oidcServer(t, f, nil)
			to, c := oidcSignIn(t, srv, f)
			if tc.want == "" {
				if c != nil || to != "/?signin=refused" || len(srv.sessions.m) != 0 {
					t.Fatalf("refused account: redirect %q, cookie %v, %d sessions", to, c, len(srv.sessions.m))
				}
				return
			}
			if c == nil || to != "/hosts" {
				t.Fatalf("redirect %q, cookie %v", to, c)
			}
			rec := get(srv.Handler(), "/api/v1/session", c)
			if !strings.Contains(rec.Body.String(), `"role":"`+tc.want+`"`) || !strings.Contains(rec.Body.String(), `"user":"Ada"`) || strings.Contains(rec.Body.String(), "token") {
				t.Fatalf("session: %s", rec.Body)
			}
		})
	}
}

func TestOIDCEmailsMustBeVerified(t *testing.T) {
	f := newFakeOIDC(t)
	delete(f.claims, "groups")
	srv := oidcServer(t, f, func(c *ProviderConfig) {
		c.AdminGroups, c.OperatorGroups, c.ViewerGroups = nil, nil, nil
		c.AdminEmails, c.ViewerEmails = []string{"Ada@Example.org"}, []string{"@example.org"}
	})
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("verified admin address refused")
	}
	f.claims["email"] = "bob@example.org"
	to, c := oidcSignIn(t, srv, f)
	if c == nil || !strings.Contains(get(srv.Handler(), "/api/v1/session", c).Body.String(), `"role":"viewer"`) {
		t.Fatalf("@domain viewer: %q %v", to, c)
	}
	f.claims["email"], f.claims["email_verified"] = "ada@example.org", false
	if to, c := oidcSignIn(t, srv, f); c != nil || to != "/?signin=refused" {
		t.Fatalf("unverified address let in: %q", to)
	}
	f.claims["email"], f.claims["email_verified"] = "ada@example.org.evil.test", true
	if _, c := oidcSignIn(t, srv, f); c != nil {
		t.Fatal("a look-alike domain got in")
	}
}

func TestOIDCGroupsFromUserinfo(t *testing.T) {
	f := newFakeOIDC(t)
	delete(f.claims, "groups")
	f.userinfo = map[string]any{"sub": "u-1", "groups": []string{"obs-admins"}}
	srv := oidcServer(t, f, nil)
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("groups in userinfo weren't read")
	}
	f.userinfo = map[string]any{"sub": "someone-else", "groups": []string{"obs-admins"}}
	if _, c := oidcSignIn(t, srv, f); c != nil {
		t.Fatal("userinfo for another subject was believed")
	}
}

func TestOIDCNestedClaim(t *testing.T) {
	f := newFakeOIDC(t)
	delete(f.claims, "groups")
	f.claims["realm_access"] = map[string]any{"roles": []string{"obs-admins"}}
	srv := oidcServer(t, f, func(c *ProviderConfig) { c.GroupsClaim = "realm_access.roles" })
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("realm_access.roles not read")
	}
}

func TestOIDCECKey(t *testing.T) {
	f := newFakeOIDC(t)
	f.useEC = true
	if _, c := oidcSignIn(t, oidcServer(t, f, nil), f); c == nil {
		t.Fatal("ES256 ID token refused")
	}
}

// Every way an ID token can be wrong ends in "failed", with no session.
func TestOIDCBadIDTokens(t *testing.T) {
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, tamper := range map[string]func(*fakeOIDC){
		"wrong issuer":   func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["iss"] = "https://evil.test" } },
		"wrong audience": func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["aud"] = "someone-else" } },
		"other azp": func(f *fakeOIDC) {
			f.tamper = func(c map[string]any) { c["aud"] = []string{"obs", "x"}; c["azp"] = "x" }
		},
		"expired":     func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() } },
		"future":      func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() } },
		"wrong nonce": func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["nonce"] = "replayed" } },
		"no nonce":    func(f *fakeOIDC) { f.tamper = func(c map[string]any) { delete(c, "nonce") } },
		"no subject":  func(f *fakeOIDC) { f.tamper = func(c map[string]any) { delete(c, "sub") } },
		"foreign key": func(f *fakeOIDC) { f.forge = other },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOIDC(t)
			tamper(f)
			srv := oidcServer(t, f, nil)
			if to, c := oidcSignIn(t, srv, f); c != nil || to != "/?signin=failed" {
				t.Fatalf("redirect %q, cookie %v", to, c)
			}
		})
	}
}

func TestOIDCRefusesUnsafeAlgorithms(t *testing.T) {
	f := newFakeOIDC(t)
	o, _ := newOIDC(oidcConfig(f))
	payload := enc([]byte(`{"iss":"` + f.srv.URL + `","aud":"obs","sub":"u-1","exp":9999999999,"iat":1}`))
	for _, alg := range []string{"none", "HS256"} {
		h := enc([]byte(`{"alg":"` + alg + `"}`))
		if _, err := o.idToken(t.Context(), h+"."+payload+"."+enc([]byte("x")), "", time.Now()); err == nil {
			t.Fatalf("alg %s accepted", alg)
		}
	}
}

func TestOIDCKeyRotation(t *testing.T) {
	f := newFakeOIDC(t)
	srv := oidcServer(t, f, nil)
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("first sign-in")
	}
	f.rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	f.kid = "k2"
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("a rotated key (new kid) wasn't fetched")
	}
}

func TestOIDCRecheck(t *testing.T) {
	ctx := context.Background()
	f := newFakeOIDC(t)
	f.userinfo = map[string]any{"sub": "u-1", "name": "Ada", "groups": []string{"obs-admins"}}
	pc := oidcConfig(f)
	_ = pc.check()
	o, _ := newOIDC(pc)
	srv := oidcServer(t, f, nil)
	_, c := oidcSignIn(t, srv, f)
	if c == nil {
		t.Fatal("sign-in")
	}
	var id *Identity
	for _, s := range srv.sessions.m {
		id = s.ident
	}
	if id.Subject != "u-1" || id.tokens.RefreshToken != "rt-1" || id.tokens.IDToken != "" {
		t.Fatalf("identity kept %+v / %+v", id, id.tokens)
	}
	// Still an admin: the refresh gives a new token and the same role.
	next, err := o.Recheck(ctx, id)
	if err != nil || next.Role != RoleAdmin || next.tokens.AccessToken != "at-2" || next.tokens.RefreshToken != "rt-1" {
		t.Fatalf("recheck: %v %+v", err, next)
	}
	// Moved to the viewers at the provider: a viewer from the next check.
	f.claims["groups"] = []string{"obs-viewers"}
	if next, err = o.Recheck(ctx, id); err != nil || next.Role != RoleViewer {
		t.Fatalf("demotion: %v %+v", err, next)
	}
	// Out of both groups: signed out.
	f.claims["groups"] = []string{}
	if _, err = o.Recheck(ctx, id); !errors.Is(err, errNotAllowed) {
		t.Fatalf("removed from the groups: %v", err)
	}
	f.claims["groups"] = []string{"obs-admins"}
	// Refresh refused (account disabled): signed out.
	f.refreshOK = false
	if _, err = o.Recheck(ctx, id); !errors.Is(err, errNotAllowed) {
		t.Fatalf("refused refresh: %v", err)
	}
	f.refreshOK = true
	// Provider down: unreachable, which the session's grace covers.
	f.down = true
	var down errUnreachable
	if _, err = o.Recheck(ctx, id); !errors.As(err, &down) {
		t.Fatalf("provider down: %v", err)
	}
	f.down = false
	// A refreshed token for someone else.
	f.tamper = func(c map[string]any) { c["sub"] = "u-2" }
	if _, err = o.Recheck(ctx, id); !errors.Is(err, errNotAllowed) {
		t.Fatalf("other subject: %v", err)
	}
	f.tamper = nil
	// No new ID token on refresh: userinfo says who it is.
	f.noIDOnRef = true
	if next, err = o.Recheck(ctx, id); err != nil || next.Role != RoleAdmin {
		t.Fatalf("userinfo recheck: %v", err)
	}
	f.userinfo = nil // userinfo now refuses the token
	if _, err = o.Recheck(ctx, id); !errors.Is(err, errNotAllowed) {
		t.Fatalf("no ID token, userinfo refuses: %v", err)
	}
	// Without a refresh token the session lasts as long as the access token.
	bare := *id
	bare.tokens = &tokens{AccessToken: "at-1", expires: time.Now().Add(time.Minute)}
	if _, err = o.Recheck(ctx, &bare); err != nil {
		t.Fatalf("access token still good: %v", err)
	}
	bare.tokens.expires = time.Now().Add(-time.Second)
	if _, err = o.Recheck(ctx, &bare); !errors.Is(err, errNotAllowed) {
		t.Fatalf("access token expired: %v", err)
	}
}

func TestOIDCClientSecretGoesInBasic(t *testing.T) {
	f := newFakeOIDC(t)
	p := t.TempDir() + "/secret"
	if err := os.WriteFile(p, []byte("s3cr:et\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := oidcServer(t, f, func(c *ProviderConfig) { c.ClientSecretFile = p })
	if _, c := oidcSignIn(t, srv, f); c == nil {
		t.Fatal("sign-in")
	}
	if f.basic != "obs:s3cr%3Aet" {
		t.Fatalf("basic auth %q, want the form-encoded id and secret", f.basic)
	}
}

func TestOIDCConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ProviderConfig)
		want string
	}{
		{"nobody", func(c *ProviderConfig) { c.AdminGroups, c.OperatorGroups, c.ViewerGroups = nil, nil, nil }, "say who gets in"},
		{"plain http", func(c *ProviderConfig) { c.Issuer = "http://idp.example.org" }, "must be https"},
		{"reserved param", func(c *ProviderConfig) { c.AuthParams = map[string]string{"redirect_uri": "https://evil.test"} }, "can't set redirect_uri"},
		{"bad address", func(c *ProviderConfig) { c.AdminEmails = []string{"example.org"} }, "not an address"},
		{"callback", func(c *ProviderConfig) { c.RedirectURL = "https://obs.test/auth/other/callback" }, "must end in /auth/sso/callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ProviderConfig{ID: "sso", Type: "oidc", Issuer: "https://idp.example.org", ClientID: "obs", RedirectURL: oidcRedirect, AdminGroups: []string{"a"}}
			tc.edit(&c)
			if err := c.check(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	c := ProviderConfig{ID: "sso", Type: "oidc", Issuer: "https://idp.example.org", ClientID: "obs", RedirectURL: oidcRedirect,
		AdminGroups: []string{"a"}, Scopes: []string{"email"}}
	if err := c.check(); err != nil || c.Scopes[0] != "openid" || c.GroupsClaim != "groups" || c.Label == "" {
		t.Fatalf("defaults: %v %+v", err, c)
	}
}
