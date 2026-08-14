// Package authhandler implements enterprise-auth's POST /internal/authorize
// endpoint -- the HTTP side of the "network boundary, not import boundary"
// pattern api/authz.HTTPAuthorizer calls into (see that package's
// doc comment). It resolves a caller's credentials (session cookie or
// service-token Bearer header) to an identity, using session.Manager for
// both -- a human session and /alerting's service token are both just
// signed tokens with a different Role claim, so one validation path
// handles both, and the Role claim (not which header carried it) is what
// determines whether the result looks like a human or a service identity.
//
// POST /internal/authorize-ingest is a sibling endpoint, same network-
// boundary shape but for a different caller (`ingest`, core/AGPL, not
// api/authz) and a different credential type (an ingest bearer token
// checked against rbacstore's ingest_credentials table, not a
// session.Manager JWT) -- see ingestCredentialValidator's doc comment.
package authhandler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sentry/sentry/enterprise/internal/session"
)

// SessionCookieName matches the name api/authz.HTTPAuthorizer's
// tests and doc comments already assume ("sentry_session").
const SessionCookieName = "sentry_session"

// Features reports which SSO mechanisms are configured -- the response
// shape /docs/phase-4-rbac-design.md's "Web UI boundary" section commits
// to ({"sso_configured", "oidc_enabled", "saml_enabled"}), so web can
// show/hide enterprise settings sections as a runtime capability check
// rather than a conditional import.
type Features struct {
	OIDCEnabled bool
	SAMLEnabled bool
}

// ingestCredentialValidator is the narrow interface POST
// /internal/authorize-ingest needs -- *rbacstore.Store is the production
// implementation. Unlike session-backed /internal/authorize, this
// endpoint validates a completely different credential type (an ingest
// bearer token, checked against enterprise/internal/rbacstore's
// ingest_credentials table, never a session.Manager-signed JWT), so it
// needs a dependency session.Manager alone can't supply.
type ingestCredentialValidator interface {
	ValidateIngestCredential(ctx context.Context, token string) (tenantID string, err error)
}

type Handler struct {
	logger            *slog.Logger
	manager           *session.Manager
	features          Features
	ingestCredentials ingestCredentialValidator
}

func New(logger *slog.Logger, manager *session.Manager, features Features, ingestCredentials ingestCredentialValidator) *Handler {
	return &Handler{logger: logger, manager: manager, features: features, ingestCredentials: ingestCredentials}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/authorize", h.handleAuthorize)
	mux.HandleFunc("GET /auth/features", h.handleFeatures)
	mux.HandleFunc("POST /internal/authorize-ingest", h.handleAuthorizeIngest)
}

type featuresResponse struct {
	SSOConfigured bool `json:"sso_configured"`
	OIDCEnabled   bool `json:"oidc_enabled"`
	SAMLEnabled   bool `json:"saml_enabled"`
}

func (h *Handler) handleFeatures(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(featuresResponse{
		SSOConfigured: h.features.OIDCEnabled || h.features.SAMLEnabled,
		OIDCEnabled:   h.features.OIDCEnabled,
		SAMLEnabled:   h.features.SAMLEnabled,
	})
}

type authorizeResponse struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
}

// handleAuthorize checks the Authorization Bearer header first (the
// service-token path /alerting uses), falling back to the session
// cookie (the human path a browser sends). Both resolve through the same
// session.Manager.Validate -- see the package doc comment for why that's
// safe: the Role claim inside the token is what determines the result,
// not which header it arrived on.
func (h *Handler) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		if c, err := r.Cookie(SessionCookieName); err == nil {
			token = c.Value
		}
	}
	if token == "" {
		http.Error(w, "no credentials presented", http.StatusUnauthorized)
		return
	}

	claims, err := h.manager.Validate(token)
	if err != nil {
		http.Error(w, "invalid or expired credentials", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(authorizeResponse{
		TenantID: claims.TenantID,
		UserID:   claims.UserID,
		Role:     claims.Role,
	})
}

type authorizeIngestResponse struct {
	TenantID string `json:"tenant_id"`
}

// handleAuthorizeIngest is ingest/internal/grpcserver.HTTPTenantResolver's
// server side -- ingest calls this once per PushBatch (with the bearer
// token the agent presented) to resolve which tenant the batch belongs
// to, the network-boundary equivalent of api/authz.HTTPAuthorizer
// calling /internal/authorize, for a different credential type.
func (h *Handler) handleAuthorizeIngest(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "no credentials presented", http.StatusUnauthorized)
		return
	}

	tenantID, err := h.ingestCredentials.ValidateIngestCredential(r.Context(), token)
	if err != nil {
		http.Error(w, "invalid ingest credential", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(authorizeIngestResponse{TenantID: tenantID})
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimPrefix(header, prefix)
}
