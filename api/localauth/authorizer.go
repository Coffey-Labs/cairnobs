package localauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/cairnobs/cairnobs/api/authz"
)

// sessionStore is the narrow interface Authorizer depends on -- *Store
// is the production implementation; tests use a fake.
type sessionStore interface {
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
}

// sessionCookieName is also read directly by handler.go (Set-Cookie on
// login/logout) and is the one piece of this package's shape web/
// needs to know about implicitly (via credentials: 'include', not by
// name -- the browser handles the cookie, JS never reads it since it's
// HttpOnly).
const sessionCookieName = "cairnobs_local_session"

// Authorizer implements api/authz.Authorizer against local_sessions --
// wiring a non-nil *Authorizer into api/cmd/api/main.go's authorizer
// variable is what turns every existing RequireRole-wrapped route in
// dashboards/agents/queryapi/aiapi from a no-op into real enforcement,
// with no changes needed to any of those handler files (see this
// package's doc comment).
type Authorizer struct {
	store sessionStore
}

func NewAuthorizer(store sessionStore) *Authorizer {
	return &Authorizer{store: store}
}

var errNoCredential = errors.New("localauth: no session credential presented")

// Authorize checks Authorization: Bearer first (cairnobsctl and other
// non-browser callers), then the session cookie (the web UI) -- same
// precedence authz.HTTPAuthorizer's caller-side forwarding implies,
// and the same reason POST /auth/login's response body returns the raw
// token alongside setting the cookie (see handler.go): one opaque
// value works both ways.
func (a *Authorizer) Authorize(r *http.Request) (authz.Identity, error) {
	raw, err := credentialFromRequest(r)
	if err != nil {
		return authz.Identity{}, err
	}

	sess, err := a.store.GetSession(r.Context(), hashToken(raw))
	if err != nil {
		return authz.Identity{}, err
	}
	return authz.Identity{TenantID: sess.TenantID, UserID: sess.UserID, Role: sess.Role}, nil
}

func credentialFromRequest(r *http.Request) (string, error) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return auth[len(prefix):], nil
		}
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}
	return "", errNoCredential
}
