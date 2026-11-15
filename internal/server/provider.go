package server

// Sign-in providers. Every way into Cairn OBS ends the same way: an Identity
// with a Cairn OBS role. Which accounts get which role is the provider's call
// (inbuxa: superuser-only permissions; OIDC: a group claim; LDAP: group
// membership; the local directory: what Cairn OBS itself stores).
//
// Redirect providers (OAuth / OIDC: inbuxa today; Okta, Entra ID, Google,
// Keycloak, Authentik, Gitea later) send the browser away and get it back
// on /auth/{id}/callback. Password providers (LDAP / Active Directory and
// the local directory, later) will check a form instead.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RoleAdmin can do everything; RoleOperator and RoleViewer (local.go) only
// look, and an operator sees everything and handles alerts.
const RoleAdmin = "admin"

// Identity is someone a provider vouched for.
type Identity struct {
	Name  string
	Email string
	Role  string
	UID   string // the local directory's account id; empty for other providers
	// Subject is the OIDC sub: a refreshed ID token must be for the same one.
	Subject string
	// tokens are the provider's own; they stay inside obs-beacond.
	tokens *tokens
}

// Provider is one way to sign in.
type Provider interface {
	ID() string
	Label() string
	// Begin returns where to send the browser. obs-beacond keeps state and verifier.
	Begin(ctx context.Context, state, verifier string) (string, error)
	// Finish turns the callback into an identity, or errNotAllowed.
	Finish(ctx context.Context, q url.Values, verifier string) (*Identity, error)
	// Recheck asks again whether the identity may still use beacon.
	Recheck(ctx context.Context, id *Identity) (*Identity, error)
	RecheckEvery() time.Duration
	Grace() time.Duration
}

// ProviderConfig is one entry of signin.providers in obs-beacond.yaml.
type ProviderConfig struct {
	ID    string `yaml:"id"`    // used in URLs: /auth/{id}/login
	Type  string `yaml:"type"`  // inbuxa, oidc, local (more later: oauth, ldap)
	Label string `yaml:"label"` // the button text

	// Redirect providers.
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
	RedirectURL      string `yaml:"redirect_url"` // must end in /auth/{id}/callback

	// The local directory: Cairn OBS's own address, which passkeys are bound to.
	Origin string `yaml:"origin"`
	// Passwords lets local accounts use a password with a TOTP code instead
	// of a passkey (never a password alone). On by itself for the built-in
	// directory; for a configured one, only when set.
	Passwords bool `yaml:"passwords"`

	// inbuxa: every one of these permissions makes an admin; failing that,
	// every one of operator_permissions (when set) makes an operator.
	AdminPermissions    []string `yaml:"admin_permissions"`
	OperatorPermissions []string `yaml:"operator_permissions"`

	// oidc: who gets which role, from the provider's claims (oidc.go).
	AdminGroups    []string          `yaml:"admin_groups"`
	OperatorGroups []string          `yaml:"operator_groups"`
	ViewerGroups   []string          `yaml:"viewer_groups"`
	AdminEmails    []string          `yaml:"admin_emails"`    // verified addresses, or @domain
	OperatorEmails []string          `yaml:"operator_emails"` // verified addresses, or @domain
	ViewerEmails   []string          `yaml:"viewer_emails"`   // verified addresses, or @domain
	GroupsClaim    string            `yaml:"groups_claim"`    // default groups; a.b reads nested claims
	Scopes         []string          `yaml:"scopes"`          // default openid email profile (+ offline_access when offered)
	AuthParams     map[string]string `yaml:"auth_params"`     // extra authorization parameters, e.g. Google's access_type: offline

	Recheck time.Duration `yaml:"recheck"` // ask the provider again this often (15m)
	Grace   time.Duration `yaml:"grace"`   // keep sessions this long while it can't be reached (24h)
}

// SigninConfig lists the ways in, in the order the sign-in page shows them.
type SigninConfig struct {
	Providers []ProviderConfig `yaml:"providers"`
}

var plannedTypes = map[string]bool{"oauth": true, "ldap": true, "saml": true}

func (c *ProviderConfig) check() error {
	if c.ID == "" || strings.ContainsAny(c.ID, "/?#& ") {
		return fmt.Errorf("sign-in provider needs a plain id, not %q", c.ID)
	}
	if c.Recheck == 0 {
		c.Recheck = 15 * time.Minute
	}
	if c.Grace == 0 {
		c.Grace = 24 * time.Hour
	}
	switch {
	case c.Type == "inbuxa":
		return c.checkInbuxa()
	case c.Type == "oidc":
		return c.checkOIDC()
	case c.Type == "local":
		return c.checkLocal()
	case plannedTypes[c.Type]:
		return fmt.Errorf("sign-in provider %s: type %q is planned but not built yet", c.ID, c.Type)
	}
	return fmt.Errorf("sign-in provider %s: unknown type %q", c.ID, c.Type)
}

func newProvider(c ProviderConfig) (Provider, error) {
	switch c.Type {
	case "inbuxa":
		return newInbuxa(c)
	case "oidc":
		return newOIDC(c)
	}
	return nil, fmt.Errorf("sign-in provider %s: unknown type %q", c.ID, c.Type)
}

// errNotAllowed: the provider answered, and this account may not use beacon.
var errNotAllowed = errors.New("not allowed to use Cairn OBS")

// errUnreachable: the provider didn't answer (or answered with a server error).
type errUnreachable struct{ err error }

func (e errUnreachable) Error() string { return "provider unreachable: " + e.err.Error() }

const oauthCookie = "beacon_oauth"

// pending is a sign-in that has gone to a provider and not come back yet.
// With a database it's kept there, so the provider's redirect back can land
// on any copy of obs-beacond.
type pending struct {
	Provider string    `json:"provider"`
	Verifier string    `json:"verifier"`
	ReturnTo string    `json:"return_to"`
	Created  time.Time `json:"created"`
}

const (
	ceremonyOAuth = "oauth"
	pendingTTL    = 10 * time.Minute
)

func (srv *Server) putPending(state string, p pending) error {
	if srv.db != nil {
		b, _ := json.Marshal(p)
		return srv.db.PutCeremony(ceremonyOAuth, state, b, p.Created.Add(pendingTTL))
	}
	srv.pendingMu.Lock()
	defer srv.pendingMu.Unlock()
	for k, v := range srv.pending {
		if time.Since(v.Created) > pendingTTL {
			delete(srv.pending, k)
		}
	}
	if len(srv.pending) > 1000 { // someone hammering /auth/.../login; keep memory bounded
		srv.pending = map[string]pending{}
	}
	srv.pending[state] = p
	return nil
}

// takePending ends a sign-in that's out at a provider: once only.
func (srv *Server) takePending(state string) (pending, bool) {
	var p pending
	if srv.db != nil {
		b, err := srv.db.TakeCeremony(ceremonyOAuth, state, time.Now())
		return p, err == nil && json.Unmarshal(b, &p) == nil
	}
	srv.pendingMu.Lock()
	defer srv.pendingMu.Unlock()
	p, ok := srv.pending[state]
	delete(srv.pending, state)
	return p, ok && time.Since(p.Created) <= pendingTTL
}

// safeReturn keeps the after-sign-in redirect on Cairn OBS itself.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.HasPrefix(p, "/auth/") {
		return "/"
	}
	return p
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// handleLogin sends the browser to a provider's sign-in page.
func (srv *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	p := srv.providers[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	state, verifier := newToken(), newToken()+newToken() // verifier: 86 characters
	to, err := p.Begin(r.Context(), state, verifier)
	if err != nil {
		log.Printf("sign-in with %s: %v", p.ID(), err)
		http.Redirect(w, r, "/?signin=unreachable", http.StatusSeeOther)
		return
	}
	if err := srv.putPending(state, pending{Provider: p.ID(), Verifier: verifier, ReturnTo: safeReturn(r.URL.Query().Get("return")), Created: time.Now()}); err != nil {
		log.Printf("sign-in with %s: %v", p.ID(), err)
		http.Redirect(w, r, "/?signin=failed", http.StatusSeeOther)
		return
	}
	// Lax, not Strict: it has to come back on the provider's cross-site redirect.
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: state, Path: "/auth/", MaxAge: 600, HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, to, http.StatusFound)
}

// handleCallback is where a provider sends the browser back.
func (srv *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	p := srv.providers[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie(oauthCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	var pend pending
	ok := false
	if err == nil && state != "" && c.Value == state {
		pend, ok = srv.takePending(state)
	}
	if !ok || pend.Provider != p.ID() {
		http.Redirect(w, r, "/?signin=expired", http.StatusSeeOther)
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("sign-in with %s: provider said %s", p.ID(), e)
		http.Redirect(w, r, "/?signin=cancelled", http.StatusSeeOther)
		return
	}
	id, err := p.Finish(r.Context(), q, pend.Verifier)
	switch {
	case errors.Is(err, errNotAllowed):
		log.Printf("sign-in with %s refused: %v", p.ID(), err)
		srv.audit("someone", "was refused", "sign-in with "+p.ID()+": the account isn't allowed into Cairn OBS")
		http.Redirect(w, r, "/?signin=refused", http.StatusSeeOther)
		return
	case err != nil:
		log.Printf("sign-in with %s: %v", p.ID(), err)
		http.Redirect(w, r, "/?signin=failed", http.StatusSeeOther)
		return
	}
	srv.sessions.put(w, r, session{User: id.Name, Email: id.Email, Role: id.Role, Provider: p.ID(), Checked: time.Now(), ident: id})
	srv.audit(id.Name, "signed in", "with "+p.ID()+" as "+id.Role)
	http.Redirect(w, r, pend.ReturnTo, http.StatusSeeOther)
}

// recheck asks the session's provider again, at most every RecheckEvery.
// inbuxa's tokens can't be revoked one by one, so this is what makes
// removing someone's admin role stick. It returns false when the session
// must end.
func (srv *Server) recheck(ctx context.Context, tok string, s session) bool {
	if s.Dev {
		return srv.cfg.DevSignin // a stored dev session dies once dev sign-in is off
	}
	if s.demo() {
		return srv.cfg.DemoSignin // and a demo visitor's once the demo is off
	}
	if srv.local != nil && s.Provider == srv.local.id {
		return srv.recheckLocal(tok, s)
	}
	p := srv.providers[s.Provider]
	if p == nil {
		return false // provider removed from the config
	}
	if time.Since(s.Checked) < p.RecheckEvery() {
		return true
	}
	id, err := p.Recheck(ctx, s.ident)
	var down errUnreachable
	switch {
	case errors.As(err, &down):
		if time.Since(s.Checked) < p.Grace() {
			log.Printf("re-check of %s deferred: %v", s.User, err)
			return true
		}
		log.Printf("session of %s ended: no successful check for %s: %v", s.User, p.Grace(), err)
		return false
	case err != nil:
		srv.audit(s.User, "was signed out", err.Error())
		return false
	}
	srv.sessions.update(tok, func(v *session) { v.Checked, v.ident, v.Role = time.Now(), id, id.Role })
	return true
}

// signinNote says under a redirect button who gets in that way.
func signinNote(typ string) string {
	switch typ {
	case "inbuxa":
		return "Only inbuxa System Administrators get in that way; other accounts are refused."
	case "oidc":
		return "Only the groups and addresses set up for it get in; other accounts are refused."
	}
	return ""
}

// providerList is what the sign-in page shows.
func (srv *Server) providerList() []map[string]any {
	out := []map[string]any{}
	for _, c := range srv.cfg.Signin.Providers {
		if p := srv.providers[c.ID]; p != nil {
			out = append(out, map[string]any{"id": p.ID(), "label": p.Label(), "kind": "redirect", "note": signinNote(c.Type)})
		}
		if srv.local != nil && c.ID == srv.local.id {
			out = append(out, srv.local.signinEntry())
		}
	}
	if srv.local != nil && srv.local.builtIn {
		out = append(out, srv.local.signinEntry())
	}
	return out
}
