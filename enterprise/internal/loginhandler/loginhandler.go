// Package loginhandler is the piece named as missing throughout Phase 4:
// the actual HTTP login/callback flow that issues a *human* session,
// not just /alerting's RoleService credential (-mint-service-token) or
// the RBAC-enforcement plumbing that assumes a session already exists.
// enterprise/internal/oidc and enterprise/internal/saml do the protocol
// mechanics (discovery/AuthnRequest generation, code exchange/assertion
// parsing, signature verification); this package is the HTTP handlers
// that drive them and decide what happens with a verified identity: look
// up or create a users row, resolve which tenant/role that user belongs
// to, and issue a session.Manager-signed session cookie. Both protocols
// share that decision (resolveIdentity below) -- only how the identity
// gets verified differs.
//
// Multi-tenant users (one identity with memberships in more than one
// tenant) get a real tenant-selection step, not a guess: finishLogin
// issues a short-lived session.Manager "pending login" token (proves
// who they are, commits to no tenant yet) and redirects to
// selectTenantRedirectURL instead of issuing a session outright.
// GET /auth/memberships and POST /auth/select-tenant complete the round
// trip. web/src/routes/select-tenant is the frontend page that calls
// them, over credentialed cross-origin fetch (see
// httpserver.WithCredentialedCORS and config.CORSAllowedOrigin) -- see
// that route's own comments for the page itself.
package loginhandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sentry/sentry/enterprise/internal/authhandler"
	"github.com/sentry/sentry/enterprise/internal/oidc"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/enterprise/internal/saml"
	"github.com/sentry/sentry/enterprise/internal/session"
)

// oidcStateCookieName carries OIDC's CSRF-protection state value between
// the login redirect and the callback -- a short-lived, scoped-to-the-
// callback-path cookie (the "double-submit cookie" pattern) rather than
// server-side state, since this service otherwise has no per-browser
// session store to put it in before a session exists.
const oidcStateCookieName = "sentry_oidc_state"

// samlRequestCookieName is SAML's analog -- carries the AuthnRequest ID
// LoginURL generated, so the ACS handler can pass it back to
// ParseResponse's possibleRequestIDs (SAML's actual replay/unsolicited-
// response defense -- see saml.ServiceProvider.LoginURL's doc comment).
const samlRequestCookieName = "sentry_saml_request"

// pendingLoginCookieName carries a PendingLoginClaims token from
// finishLogin's multi-membership branch through GET /auth/memberships
// and POST /auth/select-tenant -- Path "/auth" (not "/") so it's never
// sent on ordinary requests, only the two routes that need it.
const pendingLoginCookieName = "sentry_pending_login"

// loginCookieTTL bounds how long a user has to complete the IdP round
// trip -- generous enough for a real login form, short enough that a
// stale cookie isn't a long-lived CSRF token sitting in a browser.
// Shared by both protocols' cookies.
const loginCookieTTL = 10 * time.Minute

// userStore is the narrow interface Handler depends on -- *rbacstore.Store
// is the production implementation; tests use a fake, same pattern used
// throughout this codebase (api/dashboards, api/queryapi).
type userStore interface {
	UpsertUserBySSO(ctx context.Context, ssoSubject, email, displayName string) (*rbacstore.User, error)
	ListMembershipsForUser(ctx context.Context, userID string) ([]rbacstore.Membership, error)
	// ListMembershipsWithTenantForUser backs GET /auth/memberships --
	// the one caller that needs tenant display names, not just IDs/roles.
	ListMembershipsWithTenantForUser(ctx context.Context, userID string) ([]rbacstore.MembershipWithTenant, error)
}

// oidcProvider is the narrow slice of *oidc.Provider Handler needs --
// letting tests substitute a provider pointed at a fake IdP without
// needing real OIDC discovery against something Handler's own tests
// would have to stand up twice.
type oidcProvider interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (*oidc.Claims, error)
}

// samlProvider mirrors oidcProvider's reasoning for SAML.
type samlProvider interface {
	LoginURL(relayState string) (redirectURL, requestID string, err error)
	ParseResponse(r *http.Request, possibleRequestIDs []string) (*saml.Claims, error)
}

type Handler struct {
	logger  *slog.Logger
	oidc    oidcProvider // nil if OIDC isn't configured -- RegisterRoutes registers nothing in that case
	saml    samlProvider // nil if SAML isn't configured -- same
	session *session.Manager
	users   userStore
	// postLoginRedirectURL is where the browser lands after a session
	// cookie is set -- web's base URL in a real deployment.
	postLoginRedirectURL string
	// selectTenantRedirectURL is where the browser lands instead, when
	// the identity has more than one tenant_memberships row -- see this
	// package's doc comment.
	selectTenantRedirectURL string
}

// New takes concrete *oidc.Provider/*saml.ServiceProvider (both
// nilable), not the narrower interfaces directly -- assigning a nil
// pointer straight into an interface-typed field would produce a
// non-nil interface wrapping a nil pointer (Go's classic typed-nil
// trap), which would silently break RegisterRoutes'/the handlers'
// `h.oidc == nil`/`h.saml == nil` checks the moment a caller
// (enterprise-auth's main.go) passes a `var p *oidc.Provider` that's
// legitimately still nil because that protocol isn't configured.
// Checking the concrete pointers here, before they ever become the
// interface fields, is what keeps those checks meaningful -- see
// loginhandler_test.go's TestRegisterRoutesNoOpWithTypedNilProviderVariable
// for the regression test that caught this the first time (OIDC; SAML
// follows the same fix from day one).
func New(logger *slog.Logger, oidcProvider *oidc.Provider, samlProvider *saml.ServiceProvider, sessionManager *session.Manager, users userStore, postLoginRedirectURL, selectTenantRedirectURL string) *Handler {
	h := &Handler{logger: logger, session: sessionManager, users: users, postLoginRedirectURL: postLoginRedirectURL, selectTenantRedirectURL: selectTenantRedirectURL}
	if oidcProvider != nil {
		h.oidc = oidcProvider
	}
	if samlProvider != nil {
		h.saml = samlProvider
	}
	return h
}

// RegisterRoutes registers each protocol's routes only if that protocol
// is actually configured -- matches the "absent, not broken" default
// every other optional-config path in this codebase follows (e.g.
// api/authz.RequireRole's nil-authorizer no-op).
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	if h.oidc != nil {
		mux.HandleFunc("GET /auth/oidc/login", h.handleOIDCLogin)
		mux.HandleFunc("GET /auth/oidc/callback", h.handleOIDCCallback)
	}
	if h.saml != nil {
		mux.HandleFunc("GET /auth/saml/login", h.handleSAMLLogin)
		mux.HandleFunc("POST /auth/saml/acs", h.handleSAMLACS)
	}
	if h.oidc != nil || h.saml != nil {
		mux.HandleFunc("GET /auth/memberships", h.handleListMemberships)
		mux.HandleFunc("POST /auth/select-tenant", h.handleSelectTenant)
	}
}

func (h *Handler) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	state, err := oidc.NewState()
	if err != nil {
		h.logger.Error("generating oidc state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookieName, Value: state, Path: "/auth/oidc/callback",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: int(loginCookieTTL.Seconds()),
	})
	http.Redirect(w, r, h.oidc.AuthCodeURL(state), http.StatusFound)
}

// clearCookie is called on every path out of the two callback handlers
// below -- the state/request cookie is single-use regardless of whether
// the login ultimately succeeds, same reasoning a CSRF token gets
// discarded after one use rather than left around for reuse.
func clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

func (h *Handler) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	defer clearCookie(w, r, oidcStateCookieName, "/auth/oidc/callback")

	stateCookie, err := r.Cookie(oidcStateCookieName)
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

	h.finishLogin(w, r, claims.Subject, claims.Email)
}

func (h *Handler) handleSAMLLogin(w http.ResponseWriter, r *http.Request) {
	// relayState isn't used to carry anything here (postLoginRedirectURL
	// is a fixed server-side config, not per-request) -- still generated
	// fresh per login and round-tripped, since crewjam/saml's API expects
	// one and an empty/constant value would be a needless deviation from
	// how a real SP-initiated flow looks.
	relayState, err := oidc.NewState() // same random-value generator, protocol-agnostic despite the package name
	if err != nil {
		h.logger.Error("generating saml relay state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	redirectURL, requestID, err := h.saml.LoginURL(relayState)
	if err != nil {
		h.logger.Error("building saml login url", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// SameSiteNoneMode, not Lax like OIDC's state cookie: SAML's
	// HTTP-POST binding means the browser POSTs to /auth/saml/acs
	// *from the IdP's origin* -- a cross-site POST, which SameSite=Lax
	// cookies are never sent on (Lax only exempts top-level GET
	// navigations, which is what OIDC's redirect-based callback is, but
	// SAML's response delivery isn't). SameSite=None requires Secure
	// per the cookie spec, so this genuinely needs the deployment to be
	// on HTTPS -- realistic for any real SAML IdP integration (they
	// require it too), but worth stating plainly: unlike OIDC, SAML
	// login will not work correctly over plain HTTP.
	http.SetCookie(w, &http.Cookie{
		Name: samlRequestCookieName, Value: requestID, Path: "/auth/saml/acs",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteNoneMode,
		MaxAge: int(loginCookieTTL.Seconds()),
	})
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (h *Handler) handleSAMLACS(w http.ResponseWriter, r *http.Request) {
	defer clearCookie(w, r, samlRequestCookieName, "/auth/saml/acs")

	requestCookie, err := r.Cookie(samlRequestCookieName)
	if err != nil || requestCookie.Value == "" {
		http.Error(w, "missing or expired login state -- start over at /auth/saml/login", http.StatusBadRequest)
		return
	}

	claims, err := h.saml.ParseResponse(r, []string{requestCookie.Value})
	if err != nil {
		h.logger.Error("parsing saml response", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if claims.Email == "" {
		http.Error(w, "identity provider did not return an email attribute", http.StatusUnauthorized)
		return
	}
	if claims.NameID == "" {
		http.Error(w, "identity provider did not return a NameID", http.StatusUnauthorized)
		return
	}

	h.finishLogin(w, r, claims.NameID, claims.Email)
}

// finishLogin is the point both protocols converge on: a verified
// (subject, email) pair, still needing tenant/role resolution --
// everything from here down is protocol-agnostic.
func (h *Handler) finishLogin(w http.ResponseWriter, r *http.Request, subject, email string) {
	outcome, status, err := h.resolveIdentity(r.Context(), subject, email)
	if err != nil {
		h.logger.Error("resolving identity after login", "error", err, "email", email)
		http.Error(w, err.Error(), status)
		return
	}

	if outcome.ambiguous {
		h.startTenantSelection(w, r, outcome.userID)
		return
	}
	h.issueSessionAndRedirect(w, r, outcome.identity.tenantID, outcome.identity.userID, outcome.identity.role)
}

// issueSessionAndRedirect is finishLogin's single-membership path and
// handleSelectTenant's success path converging on the same "issue a
// real session, set the cookie, send the browser on" logic -- the only
// difference between the two callers is how they got a
// (tenantID, userID, role) tuple to issue a session for.
func (h *Handler) issueSessionAndRedirect(w http.ResponseWriter, r *http.Request, tenantID, userID, role string) {
	token, err := h.session.IssueUserSession(tenantID, userID, role)
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

// startTenantSelection issues a pending-login token for an identity
// with more than one tenant_memberships row and sends the browser to
// selectTenantRedirectURL instead of completing the login -- the real
// tenant-selection step named as a gap throughout Phase 4's docs, not a
// refusal anymore (see this package's doc comment).
func (h *Handler) startTenantSelection(w http.ResponseWriter, r *http.Request, userID string) {
	token, err := h.session.IssuePendingLogin(userID)
	if err != nil {
		h.logger.Error("issuing pending login", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: pendingLoginCookieName, Value: token, Path: "/auth",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		MaxAge: int(session.PendingLoginTTL.Seconds()),
	})
	http.Redirect(w, r, h.selectTenantRedirectURL, http.StatusFound)
}

// pendingUserID validates the pending-login cookie GET /auth/memberships
// and POST /auth/select-tenant both require, writing an error response
// and returning ok=false if it's missing, expired, or forged.
func (h *Handler) pendingUserID(w http.ResponseWriter, r *http.Request) (userID string, ok bool) {
	cookie, err := r.Cookie(pendingLoginCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "missing or expired pending login -- start over at /auth/oidc/login or /auth/saml/login", http.StatusBadRequest)
		return "", false
	}
	userID, err = h.session.ValidatePendingLogin(cookie.Value)
	if err != nil {
		http.Error(w, "missing or expired pending login -- start over at /auth/oidc/login or /auth/saml/login", http.StatusUnauthorized)
		return "", false
	}
	return userID, true
}

// membershipOption is one GET /auth/memberships response entry.
type membershipOption struct {
	TenantID          string `json:"tenant_id"`
	TenantDisplayName string `json:"tenant_display_name"`
	Role              string `json:"role"`
}

// handleListMemberships lists the pending login's tenants to choose
// from -- a tenant-picker UI's first call, once one exists (see this
// package's doc comment).
func (h *Handler) handleListMemberships(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.pendingUserID(w, r)
	if !ok {
		return
	}
	memberships, err := h.users.ListMembershipsWithTenantForUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("listing memberships with tenant", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	options := make([]membershipOption, 0, len(memberships))
	for _, m := range memberships {
		options = append(options, membershipOption{TenantID: m.TenantID, TenantDisplayName: m.TenantDisplayName, Role: string(m.Role)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(options)
}

type selectTenantRequest struct {
	TenantID string `json:"tenant_id"`
}

type selectTenantResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// handleSelectTenant completes the tenant-selection round trip: given a
// still-valid pending login and a chosen tenant_id, re-derives the
// membership/role for that specific tenant server-side (never trusting
// a client-supplied role -- only which tenant they claim to want,
// checked against their actual memberships) and issues the real
// session. Responds with JSON, not a redirect: this is a POST a
// tenant-picker page would call via fetch, which should decide for
// itself how to navigate afterward, not have a redirect response
// imposed on it the way the GET-based OIDC/SAML callbacks reasonably
// can.
func (h *Handler) handleSelectTenant(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.pendingUserID(w, r)
	if !ok {
		return
	}
	var body selectTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TenantID == "" {
		http.Error(w, `invalid request body -- expected {"tenant_id": "..."}`, http.StatusBadRequest)
		return
	}

	memberships, err := h.users.ListMembershipsForUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("listing memberships", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var role string
	found := false
	for _, m := range memberships {
		if m.TenantID == body.TenantID {
			role = string(m.Role)
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "no membership in the requested tenant", http.StatusForbidden)
		return
	}

	clearCookie(w, r, pendingLoginCookieName, "/auth")
	token, err := h.session.IssueUserSession(body.TenantID, userID, role)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(selectTenantResponse{RedirectURL: h.postLoginRedirectURL})
}

var (
	// ErrNoMembership is exported so tests (and any future caller that
	// wants to distinguish this from other failures) don't have to
	// string-match the HTTP error body.
	ErrNoMembership = errors.New("loginhandler: this identity has no tenant membership -- contact your administrator")
)

type resolvedIdentity struct {
	tenantID string
	userID   string
	role     string
}

// identityOutcome is resolveIdentity's result: exactly one membership
// resolves identity directly; more than one sets ambiguous (userID is
// always populated so the caller can start tenant selection without a
// second lookup).
type identityOutcome struct {
	identity  resolvedIdentity
	userID    string
	ambiguous bool
}

// resolveIdentity is the policy decision this whole package exists to
// make: given a verified external identity (subject, email -- OIDC's
// "sub"/"email" claims or SAML's NameID/email attribute, already
// protocol-normalized by the caller), which tenant/role does it map to.
// Zero memberships refuses outright (ErrNoMembership) -- there's
// nothing to choose between. More than one is no longer a refusal (see
// this package's doc comment): finishLogin routes an ambiguous outcome
// into tenant selection instead of erroring.
func (h *Handler) resolveIdentity(ctx context.Context, subject, email string) (identityOutcome, int, error) {
	user, err := h.users.UpsertUserBySSO(ctx, subject, email, email)
	if err != nil {
		return identityOutcome{}, http.StatusInternalServerError, fmt.Errorf("loginhandler: upserting user: %w", err)
	}

	memberships, err := h.users.ListMembershipsForUser(ctx, user.ID)
	if err != nil {
		return identityOutcome{}, http.StatusInternalServerError, fmt.Errorf("loginhandler: listing memberships: %w", err)
	}
	switch len(memberships) {
	case 0:
		return identityOutcome{}, http.StatusForbidden, ErrNoMembership
	case 1:
		return identityOutcome{identity: resolvedIdentity{tenantID: memberships[0].TenantID, userID: user.ID, role: string(memberships[0].Role)}, userID: user.ID}, 0, nil
	default:
		return identityOutcome{userID: user.ID, ambiguous: true}, 0, nil
	}
}
