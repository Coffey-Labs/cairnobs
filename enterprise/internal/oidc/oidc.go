// Package oidc wires coreos/go-oidc into a small relying-party client:
// discovery, the login redirect, and code exchange + ID token
// verification. Deliberately thin -- this package answers "is this
// person who they say they are, and what's their email/subject" and
// nothing about tenants/roles; internal/session maps a verified identity
// to a tenant.ID via tenant.TrustFromValidatedSession, kept as a
// separate concern per /docs/phase-4-isolation-design.md.
package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	goidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// Scopes beyond the mandatory "openid" -- "email" and "profile" are
	// the common additions IdPs support without extra configuration.
	Scopes []string
}

// Provider wraps a discovered OIDC issuer and the oauth2 config derived
// from it. Construction does real network discovery (GET
// {issuer}/.well-known/openid-configuration) -- see New's doc comment.
type Provider struct {
	verifier *goidc.IDTokenVerifier
	oauth2   oauth2.Config
}

// Claims is the subset of ID token claims Sentry actually uses. Extend
// deliberately, not by passing the raw claim map further up the stack --
// every field added here is a field internal/session has to decide how
// to trust.
type Claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// New performs OIDC discovery against cfg.IssuerURL. Real network I/O --
// call once at startup (or lazily, cached), not per request.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("oidc: IssuerURL, ClientID, and RedirectURL are required")
	}

	issuer, err := goidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovering issuer %q: %w", cfg.IssuerURL, err)
	}

	scopes := append([]string{goidc.ScopeOpenID}, cfg.Scopes...)

	return &Provider{
		verifier: issuer.Verifier(&goidc.Config{ClientID: cfg.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     issuer.Endpoint(),
			Scopes:       scopes,
		},
	}, nil
}

// NewState generates a CSRF-protection state value for the login
// redirect. The caller is responsible for storing it (session/cookie)
// and comparing it against what comes back to the callback endpoint --
// this package doesn't hold any server-side state itself.
func NewState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("oidc: generating state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthCodeURL is where the browser gets redirected to start login.
func (p *Provider) AuthCodeURL(state string) string {
	return p.oauth2.AuthCodeURL(state)
}

// Exchange trades an authorization code for tokens and returns the
// verified ID token's claims. Verification (signature, issuer,
// audience, expiry) happens inside p.verifier.Verify -- this is the
// step that actually establishes trust, not just "we got a token back."
func (p *Provider) Exchange(ctx context.Context, code string) (*Claims, error) {
	token, err := p.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oidc: exchanging code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("oidc: token response had no id_token")
	}

	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verifying id_token: %w", err)
	}

	var claims Claims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: decoding claims: %w", err)
	}
	return &claims, nil
}
