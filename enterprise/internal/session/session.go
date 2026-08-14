// Package session issues and validates the signed tokens enterprise-auth
// hands back to /api's authz.HTTPAuthorizer -- both human sessions
// (issued after a successful OIDC/SAML login) and the long-lived
// RoleService credential /alerting's queryclient presents as a Bearer
// token. HS256/JWT rather than a bespoke format: boring, well-understood,
// and go-jose is already a dependency via oidc.
//
// One shared signing key (ENTERPRISE_SESSION_SIGNING_KEY) issues and
// validates both kinds of token -- there is deliberately no separate key
// per token type, since the Role claim (not the key used) is what
// authz.Role.Satisfies enforces downstream.
package session

import (
	"errors"
	"fmt"
	"time"

	josev4 "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Claims mirrors api/authz.Identity's fields (TenantID, UserID,
// Role as a string) plus the standard registered JWT claims. Role is
// deliberately a plain string, not enterprise's own type, since its only
// consumer -- authz.Role -- is defined in core and this package must not
// import it (core must not import enterprise/, but the reverse also
// stays a network boundary here: this package has no reason to depend on
// api's Go types either).
type Claims struct {
	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Role     string `json:"role"`
	jwt.Claims
}

const (
	// HumanSessionTTL matches a typical browser-session lifetime; re-auth
	// happens via a fresh OIDC/SAML round trip, not silent refresh (no
	// refresh-token flow is built yet -- named future work).
	HumanSessionTTL = 12 * time.Hour
	// ServiceTokenTTL is long-lived by design: /alerting runs as a
	// continuously-deployed workload with no interactive re-auth path.
	// Rotation is by redeploying alerting with a freshly issued token,
	// not automatic refresh.
	ServiceTokenTTL = 24 * 365 * time.Hour
	// MinSigningKeyBytes: HS256 wants a key at least as long as its
	// output (32 bytes/256 bits) to not weaken the MAC.
	MinSigningKeyBytes = 32
)

// ErrInvalidToken covers every validation failure (bad signature,
// malformed token, expired) -- deliberately not distinguished further so
// callers can't be tempted to treat "expired" as a softer case than
// "forged"; both mean "do not trust this caller."
var ErrInvalidToken = errors.New("session: invalid or expired token")

type Manager struct {
	signer josev4.Signer
	key    []byte
}

func NewManager(signingKey []byte) (*Manager, error) {
	if len(signingKey) < MinSigningKeyBytes {
		return nil, fmt.Errorf("session: signing key must be at least %d bytes, got %d", MinSigningKeyBytes, len(signingKey))
	}
	signer, err := josev4.NewSigner(
		josev4.SigningKey{Algorithm: josev4.HS256, Key: signingKey},
		(&josev4.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("session: creating signer: %w", err)
	}
	return &Manager{signer: signer, key: signingKey}, nil
}

// IssueUserSession issues a human session token for a resolved
// tenant/user/role -- called only after a successful OIDC/SAML callback
// validates the caller's identity; this function trusts its inputs
// completely, same "one production call site, verified by review" shape
// as tenant.TrustFromValidatedSession.
func (m *Manager) IssueUserSession(tenantID, userID, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		TenantID: tenantID,
		UserID:   userID,
		Role:     role,
		Claims: jwt.Claims{
			Subject:  userID,
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(HumanSessionTTL)),
		},
	}
	return jwt.Signed(m.signer).Claims(claims).Serialize()
}

// IssueServiceToken issues a RoleService credential for a named machine
// caller (subject identifies which one, e.g. "alerting", for audit/
// revocation bookkeeping). TenantID/UserID are deliberately left empty:
// per /docs/phase-4-isolation-design.md's alerting↔api gap, the caller's
// tenant is resolved server-side per-request from the resource being
// acted on (alert_rules.tenant_id), never taken from the token or the
// request body -- a service token proves "this caller is alerting," not
// "this caller may act as tenant X."
func (m *Manager) IssueServiceToken(subject string) (string, error) {
	now := time.Now()
	claims := Claims{
		Role: "service",
		Claims: jwt.Claims{
			Subject:  subject,
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(ServiceTokenTTL)),
		},
	}
	return jwt.Signed(m.signer).Claims(claims).Serialize()
}

// Validate verifies signature and expiry and returns the token's claims.
// Every failure mode collapses to ErrInvalidToken -- see its doc comment.
func (m *Manager) Validate(token string) (Claims, error) {
	parsed, err := jwt.ParseSigned(token, []josev4.SignatureAlgorithm{josev4.HS256})
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := parsed.Claims(m.key, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if err := claims.Claims.Validate(jwt.Expected{}); err != nil {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}
