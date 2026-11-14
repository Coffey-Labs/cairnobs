package server

// The generic OIDC sign-in provider (type: oidc): Okta, Microsoft Entra ID,
// Google Workspace, Keycloak, Authentik, Auth0 and anything else that speaks
// OpenID Connect. The authorization-code flow with PKCE and a nonce runs
// entirely in obs-beacond; the browser sees only the provider's page and
// obs-beacond's cookie.
//
// Who gets in is obs-beacond's call, from the provider's claims: members of
// admin_groups (or holders of an admin_emails address) are admins, of
// viewer_groups / viewer_emails viewers, and everyone else is refused with
// nothing kept. An address counts only when the provider says it's
// verified. There is no "anyone the provider knows" setting.
//
// The ID token's signature is checked against the provider's JWKS, and its
// issuer, audience, expiry and nonce. Every recheck (15 minutes) uses the
// refresh token, so a group removed or an account disabled at the provider
// ends the session then. A provider that gives no refresh token gets
// sessions that end with its access token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// oidcReserved are authorization parameters obs-beacond sets itself.
var oidcReserved = map[string]bool{"response_type": true, "client_id": true, "redirect_uri": true, "scope": true,
	"state": true, "nonce": true, "code_challenge": true, "code_challenge_method": true}

func (c *ProviderConfig) checkOIDC() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("sign-in provider %s: issuer %q is not a URL", c.ID, c.Issuer)
	}
	if u.Scheme != "https" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("sign-in provider %s: issuer must be https (plain http only for a loopback test server)", c.ID)
	}
	if c.ClientID == "" || c.RedirectURL == "" {
		return fmt.Errorf("sign-in provider %s: needs client_id and redirect_url", c.ID)
	}
	if ru, err := url.Parse(c.RedirectURL); err != nil || !strings.HasSuffix(ru.Path, "/auth/"+c.ID+"/callback") {
		return fmt.Errorf("sign-in provider %s: redirect_url must end in /auth/%s/callback", c.ID, c.ID)
	}
	if len(c.groupLists())+len(c.emailLists()) == 0 {
		return fmt.Errorf("sign-in provider %s: say who gets in: admin_groups, operator_groups, viewer_groups, admin_emails, operator_emails or viewer_emails", c.ID)
	}
	for _, list := range [][]string{c.AdminEmails, c.OperatorEmails, c.ViewerEmails} {
		for i, e := range list {
			e = strings.ToLower(strings.TrimSpace(e))
			if !strings.Contains(e, "@") || strings.HasSuffix(e, "@") {
				return fmt.Errorf("sign-in provider %s: %q is not an address or @domain", c.ID, list[i])
			}
			list[i] = e
		}
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(c.Scopes, "openid") {
		c.Scopes = append([]string{"openid"}, c.Scopes...)
	}
	for k := range c.AuthParams {
		if oidcReserved[k] {
			return fmt.Errorf("sign-in provider %s: auth_params can't set %s", c.ID, k)
		}
	}
	if c.Label == "" {
		c.Label = "Sign in with single sign-on"
	}
	return nil
}

type oidcDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ScopesSupported       []string `json:"scopes_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	at                    time.Time
}

type oidc struct {
	cfg    ProviderConfig
	secret string
	hc     *http.Client

	mu      sync.Mutex
	disc    *oidcDiscovery
	keys    jwks
	keysAt  time.Time
	keysTry time.Time // the last fetch for an unknown key, so a bad kid can't hammer the provider
}

func newOIDC(cfg ProviderConfig) (*oidc, error) {
	o := &oidc{cfg: cfg, hc: &http.Client{Timeout: 10 * time.Second}}
	if cfg.ClientSecretFile != "" {
		s, err := readSecret(cfg.ClientSecretFile)
		if err != nil {
			return nil, err
		}
		o.secret = s
	}
	return o, nil
}

func (o *oidc) ID() string                  { return o.cfg.ID }
func (o *oidc) Label() string               { return o.cfg.Label }
func (o *oidc) RecheckEvery() time.Duration { return o.cfg.Recheck }
func (o *oidc) Grace() time.Duration        { return o.cfg.Grace }

func (o *oidc) getJSON(ctx context.Context, u, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return errUnreachable{err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s refused the token", errNotAllowed, o.cfg.ID)
	case resp.StatusCode >= 500:
		return errUnreachable{fmt.Errorf("%s: %s", u, resp.Status)}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	return nil
}

func (o *oidc) discover(ctx context.Context) (*oidcDiscovery, error) {
	o.mu.Lock()
	if o.disc != nil && time.Since(o.disc.at) < 10*time.Minute {
		d := o.disc
		o.mu.Unlock()
		return d, nil
	}
	o.mu.Unlock()
	var d oidcDiscovery
	if err := o.getJSON(ctx, strings.TrimRight(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", "", &d); err != nil {
		return nil, err
	}
	// The issuer it names must be the one configured (a trailing slash aside):
	// ID tokens are checked against it.
	if strings.TrimRight(d.Issuer, "/") != strings.TrimRight(o.cfg.Issuer, "/") {
		return nil, fmt.Errorf("discovery names issuer %q, not %q", d.Issuer, o.cfg.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, errors.New("discovery lacks the authorization, token or JWKS address")
	}
	d.at = time.Now()
	o.mu.Lock()
	o.disc = &d
	o.mu.Unlock()
	return &d, nil
}

// key finds the signing key for a token, fetching the JWKS when it's old or
// doesn't have the key (providers rotate keys).
func (o *oidc) key(ctx context.Context, h jwsHeader) (any, error) {
	o.mu.Lock()
	set, fresh := o.keys, time.Since(o.keysAt) < time.Hour
	o.mu.Unlock()
	if fresh {
		if k, err := pickKey(set, h); err == nil {
			return k, nil
		}
		o.mu.Lock()
		if time.Since(o.keysTry) < time.Minute {
			o.mu.Unlock()
			return nil, errNoKey
		}
		o.keysTry = time.Now()
		o.mu.Unlock()
	}
	d, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	var got jwks
	if err := o.getJSON(ctx, d.JWKSURI, "", &got); err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.keys, o.keysAt = got, time.Now()
	o.mu.Unlock()
	return pickKey(got, h)
}

// claims is a decoded token or userinfo answer.
type claims map[string]any

func (c claims) str(k string) string {
	s, _ := c[k].(string)
	return s
}

// verified says whether the provider vouches for the email claim; some send
// "true" as a string.
func (c claims) verified() bool {
	switch v := c["email_verified"].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// strings reads a claim that is a list of strings, or one string. A name
// with dots that isn't a claim itself is a path into nested claims
// (Keycloak's realm_access.roles).
func (c claims) strings(name string) ([]string, bool) {
	v, ok := c[name]
	if !ok && strings.Contains(name, ".") {
		var cur any = map[string]any(c)
		for _, part := range strings.Split(name, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				return nil, false
			}
			if cur, ok = m[part]; !ok {
				return nil, false
			}
		}
		v = cur
	}
	if !ok {
		return nil, false
	}
	switch v := v.(type) {
	case string:
		return []string{v}, true
	case []any:
		var out []string
		for _, x := range v {
			if s, isStr := x.(string); isStr {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, true
}

// idToken checks an ID token and returns its claims. nonce is empty on a
// refresh (the token then carries none, or the first one).
func (o *oidc) idToken(ctx context.Context, raw, nonce string, now time.Time) (claims, error) {
	h, payload, signed, sig, err := parseJWS(raw)
	if err != nil {
		return nil, err
	}
	if jwsAlgs[h.Alg] == "" {
		return nil, fmt.Errorf("ID token signed with %q; obs-beacond takes RS*, PS*, ES* and EdDSA", h.Alg)
	}
	key, err := o.key(ctx, h)
	if err != nil {
		return nil, err
	}
	if err := verifyJWS(h.Alg, key, signed, sig); err != nil {
		return nil, err
	}
	var c claims
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("bad ID token claims")
	}
	d, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	if c.str("iss") != d.Issuer {
		return nil, fmt.Errorf("ID token from issuer %q, not %q", c.str("iss"), d.Issuer)
	}
	aud, _ := c.strings("aud")
	if !slices.Contains(aud, o.cfg.ClientID) {
		return nil, errors.New("ID token isn't for this client")
	}
	if azp := c.str("azp"); (len(aud) > 1 || azp != "") && azp != o.cfg.ClientID {
		return nil, errors.New("ID token was issued to another client (azp)")
	}
	exp, err1 := numClaim(c, "exp")
	iat, err2 := numClaim(c, "iat")
	if err1 != nil || err2 != nil {
		return nil, errors.New("ID token lacks exp or iat")
	}
	const skew = 2 * time.Minute
	if now.After(time.Unix(exp, 0).Add(skew)) {
		return nil, errors.New("ID token has expired")
	}
	if time.Unix(iat, 0).After(now.Add(skew)) {
		return nil, errors.New("ID token issued in the future (clock skew?)")
	}
	if nonce != "" && c.str("nonce") != nonce {
		return nil, errors.New("ID token's nonce doesn't match this sign-in")
	}
	if c.str("sub") == "" {
		return nil, errors.New("ID token has no subject")
	}
	return c, nil
}

func numClaim(c claims, k string) (int64, error) {
	switch v := c[k].(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		return int64(f), err
	}
	return 0, fmt.Errorf("no %s", k)
}

// oidcNonce ties the ID token to the sign-in that started it, derived from
// the PKCE verifier obs-beacond already keeps for it.
func oidcNonce(verifier string) string { return pkceChallenge("nonce:" + verifier) }

func (o *oidc) Begin(ctx context.Context, state, verifier string) (string, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	scopes := slices.Clone(o.cfg.Scopes)
	// offline_access gets a refresh token, which the recheck needs, from
	// providers that list it (Okta, Entra ID, Keycloak, Auth0, Authentik).
	if slices.Contains(d.ScopesSupported, "offline_access") && !slices.Contains(scopes, "offline_access") {
		scopes = append(scopes, "offline_access")
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.cfg.ClientID},
		"redirect_uri":          {o.cfg.RedirectURL},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"nonce":                 {oidcNonce(verifier)},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	for k, v := range o.cfg.AuthParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// token calls the token endpoint, authenticating as the client the way the
// provider says it takes (HTTP Basic unless it lists only the form).
func (o *oidc) token(ctx context.Context, form url.Values) (*tokens, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	basic := o.secret != "" && (len(d.TokenAuthMethods) == 0 || slices.Contains(d.TokenAuthMethods, "client_secret_basic"))
	if !basic {
		form.Set("client_id", o.cfg.ClientID)
		if o.secret != "" {
			form.Set("client_secret", o.secret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if basic {
		req.SetBasicAuth(url.QueryEscape(o.cfg.ClientID), url.QueryEscape(o.secret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, errUnreachable{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, errUnreachable{fmt.Errorf("token endpoint: %s", resp.Status)}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("token endpoint: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var t tokens
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	t.expires = time.Now().Add(time.Hour)
	if t.ExpiresIn > 0 {
		t.expires = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return &t, nil
}

// needs says which claims the role decision reads.
func (o *oidc) needs(c claims) (groups, email bool) {
	if len(o.cfg.groupLists()) > 0 {
		_, has := c.strings(o.cfg.GroupsClaim)
		groups = !has
	}
	if len(o.cfg.emailLists()) > 0 {
		email = c.str("email") == "" || c["email_verified"] == nil
	}
	return
}

// withUserinfo fills in from userinfo what the ID token lacks: some
// providers put groups or the address only there. When the role depends on
// what's missing, userinfo must answer; for a display name it's best effort.
func (o *oidc) withUserinfo(ctx context.Context, c claims, accessToken string) (claims, error) {
	groups, email := o.needs(c)
	needed := groups || email
	if !needed && c.str("name") != "" && c.str("email") != "" {
		return c, nil
	}
	d, err := o.discover(ctx)
	if err != nil || d.UserinfoEndpoint == "" {
		return c, nil // if the role needed it, role says what's missing
	}
	var ui claims
	if err := o.getJSON(ctx, d.UserinfoEndpoint, accessToken, &ui); err != nil {
		if needed {
			return nil, err
		}
		return c, nil
	}
	if ui.str("sub") != c.str("sub") {
		return nil, fmt.Errorf("%w: userinfo is for another subject", errNotAllowed)
	}
	for k, v := range ui {
		if _, have := c[k]; !have {
			c[k] = v
		}
	}
	return c, nil
}

func (o *oidc) userinfoOnly(ctx context.Context, c claims, accessToken string) (claims, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	if d.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("%w: %s sent no ID token on refresh and has no userinfo to check with", errNotAllowed, o.cfg.ID)
	}
	var ui claims
	if err := o.getJSON(ctx, d.UserinfoEndpoint, accessToken, &ui); err != nil {
		return nil, err
	}
	if ui.str("sub") != c.str("sub") {
		return nil, fmt.Errorf("%w: userinfo is for another subject", errNotAllowed)
	}
	return ui, nil
}

// role decides from the claims: admin before viewer, groups or a verified
// address, or nobody.
func (o *oidc) role(c claims) (string, error) {
	groups, _ := c.strings(o.cfg.GroupsClaim)
	email := strings.ToLower(c.str("email"))
	byEmail := func(list []string) bool {
		if email == "" || !c.verified() {
			return false
		}
		for _, e := range list {
			if e == email || (strings.HasPrefix(e, "@") && strings.HasSuffix(email, e)) {
				return true
			}
		}
		return false
	}
	inGroup := func(list []string) bool {
		for _, g := range list {
			if slices.Contains(groups, g) {
				return true
			}
		}
		return false
	}
	switch {
	case inGroup(o.cfg.AdminGroups) || byEmail(o.cfg.AdminEmails):
		return RoleAdmin, nil
	case inGroup(o.cfg.OperatorGroups) || byEmail(o.cfg.OperatorEmails):
		return RoleOperator, nil
	case inGroup(o.cfg.ViewerGroups) || byEmail(o.cfg.ViewerEmails):
		return RoleViewer, nil
	}
	if _, has := c.strings(o.cfg.GroupsClaim); !has && len(o.cfg.groupLists()) > 0 {
		return "", fmt.Errorf("%w: %s sent no %q claim: the account is in no groups, or the provider isn't set to send them", errNotAllowed, o.cfg.ID, o.cfg.GroupsClaim)
	}
	return "", fmt.Errorf("%w: not in an admin, operator or viewer group or address", errNotAllowed)
}

// groupLists and emailLists are the role lists set, of each kind, joined.
func (c ProviderConfig) groupLists() []string {
	return slices.Concat(c.AdminGroups, c.OperatorGroups, c.ViewerGroups)
}

func (c ProviderConfig) emailLists() []string {
	return slices.Concat(c.AdminEmails, c.OperatorEmails, c.ViewerEmails)
}

func displayName(c claims) string {
	for _, k := range []string{"name", "preferred_username", "email"} {
		if s := c.str(k); s != "" {
			return s
		}
	}
	return c.str("sub")
}

// identity checks tokens from the token endpoint and makes the identity.
// sub is the subject the session already has (on a refresh), or empty.
func (o *oidc) identity(ctx context.Context, t *tokens, nonce, sub string) (*Identity, error) {
	var c claims
	var err error
	switch {
	case t.IDToken != "":
		if c, err = o.idToken(ctx, t.IDToken, nonce, time.Now()); err != nil {
			return nil, err
		}
	case sub != "":
		// Some providers send no new ID token on a refresh: userinfo, with
		// the new access token, is then what says who this is now.
		c = claims{"sub": sub}
		if c, err = o.userinfoOnly(ctx, c, t.AccessToken); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("the token endpoint sent no ID token (is openid in scopes?)")
	}
	if sub != "" && c.str("sub") != sub {
		return nil, fmt.Errorf("%w: refreshed ID token is for another subject", errNotAllowed)
	}
	if c, err = o.withUserinfo(ctx, c, t.AccessToken); err != nil {
		return nil, err
	}
	role, err := o.role(c)
	if err != nil {
		return nil, err
	}
	t.IDToken = "" // checked; not kept
	return &Identity{Name: displayName(c), Email: c.str("email"), Role: role, Subject: c.str("sub"), tokens: t}, nil
}

func (o *oidc) Finish(ctx context.Context, q url.Values, verifier string) (*Identity, error) {
	t, err := o.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {verifier},
		"redirect_uri":  {o.cfg.RedirectURL},
	})
	if err != nil {
		return nil, err
	}
	id, err := o.identity(ctx, t, oidcNonce(verifier), "")
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		log.Printf("sign-in with %s: no refresh token, so %s's session ends with the access token (%s); see docs/formats.md, Sign-in", o.cfg.ID, id.Name, time.Until(t.expires).Round(time.Minute))
	}
	return id, nil
}

// Recheck refreshes the tokens and decides again from the new ID token, so
// a group removed or an account disabled at the provider ends the session.
// Without a refresh token the session lasts as long as the access token.
func (o *oidc) Recheck(ctx context.Context, id *Identity) (*Identity, error) {
	if id == nil || id.tokens == nil || id.Subject == "" {
		return nil, fmt.Errorf("%w: nothing to check with", errNotAllowed)
	}
	old := *id.tokens
	if old.RefreshToken == "" {
		if time.Now().Before(old.expires) {
			return id, nil
		}
		return nil, fmt.Errorf("%w: %s's access token expired and there's no refresh token; sign in again", errNotAllowed, o.cfg.ID)
	}
	t, err := o.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}})
	var down errUnreachable
	switch {
	case errors.As(err, &down):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: refresh refused: %v", errNotAllowed, err)
	}
	if t.RefreshToken == "" {
		t.RefreshToken = old.RefreshToken
	}
	return o.identity(ctx, t, "", id.Subject)
}
