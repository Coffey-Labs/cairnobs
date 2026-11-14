package server

// The inbuxa sign-in provider: OAuth authorization code with PKCE against
// the inbuxa mail server, run entirely by obs-beacond.
//
// Who gets in: an account whose inbuxa permissions (GET /api/account)
// include every one of admin_permissions. The defaults are permissions
// inbuxa gives only to System Administrators (inbuxa-server
// crates/common/src/auth/permissions.rs: tenant administrators never get
// them), so a tenant admin, a helpdesk role or a normal user is refused and
// nothing about them is kept.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultAdminPermissions are given by inbuxa to System Administrators only.
var DefaultAdminPermissions = []string{"impersonate", "liveMetrics", "sysLegalHoldGet"}

func (c *ProviderConfig) checkInbuxa() error {
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
	c.Issuer = strings.TrimRight(c.Issuer, "/")
	if len(c.AdminPermissions) == 0 {
		c.AdminPermissions = DefaultAdminPermissions
	}
	if c.Label == "" {
		c.Label = "Sign in with inbuxa"
	}
	return nil
}

// tokens is what an OAuth token endpoint returns.
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"` // OIDC; checked, never kept
	expires      time.Time
}

type discovery struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	at                    time.Time
}

type inbuxa struct {
	cfg    ProviderConfig
	secret string
	hc     *http.Client

	mu   sync.Mutex
	disc *discovery
}

func newInbuxa(cfg ProviderConfig) (*inbuxa, error) {
	a := &inbuxa{cfg: cfg, hc: &http.Client{Timeout: 10 * time.Second}}
	if cfg.ClientSecretFile != "" {
		s, err := readSecret(cfg.ClientSecretFile)
		if err != nil {
			return nil, err
		}
		a.secret = s
	}
	return a, nil
}

func (a *inbuxa) ID() string                  { return a.cfg.ID }
func (a *inbuxa) Label() string               { return a.cfg.Label }
func (a *inbuxa) RecheckEvery() time.Duration { return a.cfg.Recheck }
func (a *inbuxa) Grace() time.Duration        { return a.cfg.Grace }

func (a *inbuxa) discover(ctx context.Context) (*discovery, error) {
	a.mu.Lock()
	if a.disc != nil && time.Since(a.disc.at) < 10*time.Minute {
		d := a.disc
		a.mu.Unlock()
		return d, nil
	}
	a.mu.Unlock()
	var d discovery
	if err := a.getJSON(ctx, a.cfg.Issuer+"/.well-known/openid-configuration", "", &d); err != nil {
		return nil, err
	}
	base, _ := url.Parse(a.cfg.Issuer + "/")
	abs := func(s string) string { // inbuxa may answer with relative endpoints
		if s == "" {
			return ""
		}
		u, err := base.Parse(s)
		if err != nil {
			return s
		}
		return u.String()
	}
	d.AuthorizationEndpoint, d.TokenEndpoint, d.UserinfoEndpoint = abs(d.AuthorizationEndpoint), abs(d.TokenEndpoint), abs(d.UserinfoEndpoint)
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errUnreachable{errors.New("discovery lacks authorization or token endpoint")}
	}
	d.at = time.Now()
	a.mu.Lock()
	a.disc = &d
	a.mu.Unlock()
	return &d, nil
}

func (a *inbuxa) getJSON(ctx context.Context, u, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return errUnreachable{err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: inbuxa refused the token", errNotAllowed)
	case resp.StatusCode >= 500:
		return errUnreachable{fmt.Errorf("%s: %s", u, resp.Status)}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func (a *inbuxa) token(ctx context.Context, form url.Values) (*tokens, error) {
	d, err := a.discover(ctx)
	if err != nil {
		return nil, err
	}
	form.Set("client_id", a.cfg.ClientID)
	if a.secret != "" {
		form.Set("client_secret", a.secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
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

// role asks inbuxa which permissions the token's account holds.
func (a *inbuxa) role(ctx context.Context, accessToken string) (string, error) {
	var acct struct {
		Permissions []string `json:"permissions"`
	}
	if err := a.getJSON(ctx, a.cfg.Issuer+"/api/account", accessToken, &acct); err != nil {
		return "", err
	}
	have := map[string]bool{}
	for _, p := range acct.Permissions {
		have[p] = true
	}
	all := func(list []string) bool {
		for _, p := range list {
			if !have[p] {
				return false
			}
		}
		return len(list) > 0
	}
	switch {
	case all(a.cfg.AdminPermissions):
		return RoleAdmin, nil
	case all(a.cfg.OperatorPermissions):
		return RoleOperator, nil
	}
	return "", fmt.Errorf("%w: not an inbuxa System Administrator", errNotAllowed)
}

// who asks for a name to show; it never decides access. userinfo first;
// inbuxa doesn't offer the email or profile scopes, so if that comes back
// bare, the JMAP session's username (the account's address) stands in.
func (a *inbuxa) who(ctx context.Context, accessToken string) (name, email string) {
	if d, err := a.discover(ctx); err == nil && d.UserinfoEndpoint != "" {
		var ui struct {
			Name              string `json:"name"`
			PreferredUsername string `json:"preferred_username"`
			Email             string `json:"email"`
		}
		if a.getJSON(ctx, d.UserinfoEndpoint, accessToken, &ui) == nil {
			switch {
			case ui.Name != "":
				return ui.Name, ui.Email
			case ui.PreferredUsername != "":
				return ui.PreferredUsername, ui.Email
			case ui.Email != "":
				return ui.Email, ui.Email
			}
		}
	}
	var js struct {
		Username string `json:"username"`
	}
	if a.getJSON(ctx, a.cfg.Issuer+"/jmap/session", accessToken, &js) == nil && js.Username != "" {
		return js.Username, js.Username
	}
	return "inbuxa administrator", ""
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *inbuxa) Begin(ctx context.Context, state, verifier string) (string, error) {
	d, err := a.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.cfg.ClientID},
		"redirect_uri":          {a.cfg.RedirectURL},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	// offline_access gets a refresh token, which the 15-minute re-check needs
	// once the access token expires; inbuxa offers openid and offline_access
	// but not email or profile.
	var scopes []string
	for _, s := range []string{"openid", "offline_access", "email", "profile"} {
		for _, have := range d.ScopesSupported {
			if s == have {
				scopes = append(scopes, s)
			}
		}
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	return d.AuthorizationEndpoint + "?" + q.Encode(), nil
}

func (a *inbuxa) Finish(ctx context.Context, q url.Values, verifier string) (*Identity, error) {
	t, err := a.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {verifier},
		"redirect_uri":  {a.cfg.RedirectURL},
	})
	if err != nil {
		return nil, err
	}
	role, err := a.role(ctx, t.AccessToken)
	if err != nil {
		return nil, err
	}
	name, email := a.who(ctx, t.AccessToken)
	return &Identity{Name: name, Email: email, Role: role, tokens: t}, nil
}

func (a *inbuxa) Recheck(ctx context.Context, id *Identity) (*Identity, error) {
	if id == nil || id.tokens == nil {
		return nil, fmt.Errorf("%w: no tokens to check with", errNotAllowed)
	}
	t := *id.tokens
	if time.Now().After(t.expires.Add(-time.Minute)) && t.RefreshToken != "" {
		nt, err := a.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
		var down errUnreachable
		switch {
		case errors.As(err, &down):
			return nil, err
		case err != nil:
			return nil, fmt.Errorf("%w: refresh refused: %v", errNotAllowed, err)
		}
		if nt.RefreshToken == "" {
			nt.RefreshToken = t.RefreshToken
		}
		t = *nt
	}
	role, err := a.role(ctx, t.AccessToken)
	if err != nil {
		return nil, err
	}
	next := *id
	next.Role, next.tokens = role, &t
	return &next, nil
}
