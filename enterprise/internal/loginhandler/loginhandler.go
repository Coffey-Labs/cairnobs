// Package loginhandler is the piece named as missing throughout Phase 4:
// the actual HTTP login/callback flow that issues a *human* session,
// not just /alerting's RoleService credential (-mint-service-token) or
// the RBAC-enforcement plumbing that assumes a session already exists.
// enterprise/internal/oidc does the OAuth2/OIDC protocol mechanics
// (discovery, the auth-code redirect, code exchange, ID token
// verification); this package is the two HTTP handlers that drive it
// and decide what happens with a verified identity: look up or create a
// users row, resolve which tenant/role that user belongs to, and issue
// a session.Manager-signed session cookie.
//
// Deliberately out of scope here: SAML's equivalent (ACS endpoint) --
// same shape, not yet built, following this package's pattern once it
// is. Multi-tenant users (one identity with memberships in more than
// one tenant) are refused with a clear error rather than guessing which
// tenant to log them into -- a tenant-selection step is real,
// undesigned future work, not silently approximated.
package loginhandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sentry/sentry/enterprise/internal/authhandler"
	"github.com/sentry/sentry/enterprise/internal/oidc"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/enterprise/internal/session"
)

// stateCookieName carries the CSRF-protection state value between the
// login redirect and the callback -- a short-lived, scoped-to-the-
// callback-path cookie (the "double-submit cookie" pattern) rather than
// server-side state, since this service otherwise has no per-browser
// session store to put it in before a session exists.
const stateCookieName = "sentry_oidc_state"

// stateCookieTTL bounds how long a user has to complete the IdP round
// trip -- generous enough for a real login form, short enough that a
// stale state cookie isn't a long-lived CSRF token sitting in a browser.
const stateCookieTTL = 10 * time.Minute

// userStore is the narrow interface Handler depends on -- *rbacstore.Store
// is the production implementation; tests use a fake, same pattern used
// throughout this codebase (api/dashboards, api/queryapi).
type userStore interface {
	UpsertUserBySSO(ctx context.Context, ssoSubject, email, displayName string) (*rbacstore.User, error)
	ListMembershipsForUser(ctx context.Context, userID string) ([]rbacstore.Membership, error)
}

// oidcProvider is the narrow slice of *oidc.Provider Handler needs --
// letting tests substitute a provider pointed at a fake IdP without
// needing real OIDC discovery against something Handler's own tests
// would have to stand up twice.
type oidcProvider interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (*oidc.Claims, error)
}

type Handler struct {
	logger  *slog.Logger
	oidc    oidcProvider // nil if OIDC isn't configured -- RegisterRoutes registers nothing in that case
	session *session.Manager
	users   userStore
	// postLoginRedirectURL is where the browser lands after a session
	// cookie is set -- web's base URL in a real deployment.
	postLoginRedirectURL string
}

// New takes a concrete *oidc.Provider (nilable), not the oidcProvider
// interface directly -- a nil *oidc.Provider assigned straight into an
// interface-typed field would produce a non-nil interface wrapping a
// nil pointer (Go's classic typed-nil trap), which would silently break
// RegisterRoutes'/handleLogin's `h.oidc == nil` checks the moment a
// caller (enterprise-auth's main.go) passes a `var p *oidc.Provider`
// that's legitimately still nil because OIDC isn't configured. Checking
// the concrete pointer here, before it ever becomes the interface
// field, is what keeps that check meaningful.
func New(logger *slog.Logger, provider *oidc.Provider, sessionManager *session.Manager, users userStore, postLoginRedirectURL string) *Handler {
	h := &Handler{logger: logger, session: sessionManager, users: users, postLoginRedirectURL: postLoginRedirectURL}
	if provider != nil {
		h.oidc = provider
	}
	return h
}

// RegisterRoutes registers OIDC's two routes only if OIDC is actually
// configured (h.oidc != nil) -- matches the "absent, not broken" default
// every other optional-config path in this codebase follows (e.g.
// api/authz.RequireRole's nil-authorizer no-op).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	if h.oidc == nil {
		return
	}
	mux.HandleFunc("GET /auth/oidc/login", h.handleLogin)
	mux.HandleFunc("GET /auth/oidc/callback", h.handleCallback)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := oidc.NewState()
	if err != nil {
		h.logger.Error("generating oidc state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: state, Path: "/auth/oidc/callback",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: int(stateCookieTTL.Seconds()),
	})
	http.Redirect(w, r, h.oidc.AuthCodeURL(state), http.StatusFound)
}

// clearStateCookie is called on every path out of handleCallback --
// the state cookie is single-use regardless of whether the login
// ultimately succeeds, same reasoning a CSRF token gets discarded after
// one use rather than left around for reuse.
func clearStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/auth/oidc/callback",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	defer clearStateCookie(w, r)

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing or expired login state -- start over at /auth/oidc/login", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "state mismatch -- possible CSRF, start over at /auth/oidc/login", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	claims, err := h.oidc.Exchange(r.Context(), code)
	if err != nil {
		h.logger.Error("exchanging oidc code", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if claims.Email == "" {
		http.Error(w, "identity provider did not return an email claim", http.StatusUnauthorized)
		return
	}

	identity, status, err := h.resolveIdentity(r.Context(), claims)
	if err != nil {
		h.logger.Error("resolving identity after oidc login", "error", err, "email", claims.Email)
		http.Error(w, err.Error(), status)
		return
	}

	token, err := h.session.IssueUserSession(identity.tenantID, identity.userID, identity.role)
	if err != nil {
		h.logger.Error("issuing session", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: authhandler.SessionCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: int(session.HumanSessionTTL.Seconds()),
	})
	http.Redirect(w, r, h.postLoginRedirectURL, http.StatusFound)
}

var (
	// ErrNoMembership and ErrMultipleMemberships are exported so tests
	// (and any future caller that wants to distinguish these outcomes,
	// e.g. to render a real tenant-picker UI instead of a flat error
	// page) don't have to string-match handleCallback's HTTP error body.
	ErrNoMembership        = errors.New("loginhandler: this identity has no tenant membership -- contact your administrator")
	ErrMultipleMemberships = errors.New("loginhandler: this identity belongs to multiple tenants -- tenant selection is not supported yet")
)

type resolvedIdentity struct {
	tenantID string
	userID   string
	role     string
}

// resolveIdentity is the policy decision this whole package exists to
// make: given a verified external identity, which tenant/role does it
// map to. Deliberately conservative -- exactly one tenant_memberships
// row is the only case handled; zero or multiple both refuse rather
// than guess (see this package's doc comment).
func (h *Handler) resolveIdentity(ctx context.Context, claims *oidc.Claims) (resolvedIdentity, int, error) {
	user, err := h.users.UpsertUserBySSO(ctx, claims.Subject, claims.Email, claims.Email)
	if err != nil {
		return resolvedIdentity{}, http.StatusInternalServerError, fmt.Errorf("loginhandler: upserting user: %w", err)
	}

	memberships, err := h.users.ListMembershipsForUser(ctx, user.ID)
	if err != nil {
		return resolvedIdentity{}, http.StatusInternalServerError, fmt.Errorf("loginhandler: listing memberships: %w", err)
	}
	switch len(memberships) {
	case 0:
		return resolvedIdentity{}, http.StatusForbidden, ErrNoMembership
	case 1:
		return resolvedIdentity{tenantID: memberships[0].TenantID, userID: user.ID, role: string(memberships[0].Role)}, 0, nil
	default:
		return resolvedIdentity{}, http.StatusNotImplemented, ErrMultipleMemberships
	}
}
