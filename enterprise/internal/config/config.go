// Package config loads enterprise-auth's configuration from environment
// variables, same convention as every other Go service in this repo.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	HTTPListenAddr    string
	Postgres          PostgresConfig
	OIDC              OIDCConfig
	SAML              SAMLConfig
	SessionSigningKey []byte
	// PostLoginRedirectURL is where the browser lands after
	// internal/loginhandler sets a session cookie -- web's base URL in
	// a real deployment.
	PostLoginRedirectURL string
	// SelectTenantRedirectURL is where the browser lands after a login
	// resolves to more than one tenant_memberships row --
	// internal/loginhandler issues a pending-login cookie and sends the
	// browser here instead of straight to PostLoginRedirectURL. Nothing
	// serves this route yet (a real tenant-picker page is undesigned
	// frontend work -- see internal/loginhandler's package doc comment);
	// the backend protocol (GET /auth/memberships, POST
	// /auth/select-tenant) is complete and independently testable via
	// HTTP regardless of what, if anything, is listening here today.
	SelectTenantRedirectURL string
}

type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

// OIDCConfig is optional -- a deployment might configure OIDC, SAML,
// both, or (during early rollout) neither yet. Load() doesn't fail if
// these are unset; internal/oidc.New is only called once IssuerURL is
// actually present.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// SAMLConfig is likewise optional. cmd/enterprise-auth/main.go fetches
// and parses IDPMetadataURL into the *saml.EntityDescriptor
// internal/saml.New requires at startup (crewjam/saml's
// samlsp.FetchMetadata) -- this struct just carries the raw config
// values this package's job (env-var loading) is scoped to.
type SAMLConfig struct {
	EntityID       string
	ACSURL         string
	IDPMetadataURL string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPListenAddr: getenv("HTTP_LISTEN_ADDR", ":8082"),
		Postgres: PostgresConfig{
			Addr:     getenv("POSTGRES_ADDR", "localhost:5432"),
			Database: getenv("POSTGRES_DATABASE", "sentry_metadata"),
			Username: getenv("POSTGRES_USERNAME", "sentry"),
			Password: getenv("POSTGRES_PASSWORD", ""),
		},
		OIDC: OIDCConfig{
			IssuerURL:    getenv("OIDC_ISSUER_URL", ""),
			ClientID:     getenv("OIDC_CLIENT_ID", ""),
			ClientSecret: getenv("OIDC_CLIENT_SECRET", ""),
			RedirectURL:  getenv("OIDC_REDIRECT_URL", ""),
		},
		SAML: SAMLConfig{
			EntityID:       getenv("SAML_ENTITY_ID", ""),
			ACSURL:         getenv("SAML_ACS_URL", ""),
			IDPMetadataURL: getenv("SAML_IDP_METADATA_URL", ""),
		},
		PostLoginRedirectURL: getenv("POST_LOGIN_REDIRECT_URL", "http://localhost:3000"),
	}
	// Defaults relative to PostLoginRedirectURL if not set explicitly --
	// computed after cfg.PostLoginRedirectURL above so a caller
	// overriding just POST_LOGIN_REDIRECT_URL still gets a sensible
	// SelectTenantRedirectURL without also having to set the new
	// variable.
	cfg.SelectTenantRedirectURL = getenv("SELECT_TENANT_REDIRECT_URL", cfg.PostLoginRedirectURL+"/select-tenant")

	// Required, unlike OIDC/SAML above: every enterprise-auth deployment
	// issues and validates session/service tokens (internal/session),
	// even one that hasn't configured any IdP yet. 32 bytes matches
	// internal/session.MinSigningKeyBytes -- not imported here to avoid
	// a config->session dependency for one constant, but the two values
	// must be kept in sync.
	signingKey := getenv("ENTERPRISE_SESSION_SIGNING_KEY", "")
	if len(signingKey) < 32 {
		return Config{}, fmt.Errorf("ENTERPRISE_SESSION_SIGNING_KEY must be set to at least 32 bytes (got %d)", len(signingKey))
	}
	cfg.SessionSigningKey = []byte(signingKey)

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
