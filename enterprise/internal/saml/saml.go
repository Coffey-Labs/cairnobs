// Package saml wires crewjam/saml into a small SP (service provider)
// client: build the login redirect, and validate/parse an incoming
// assertion. Deliberately not using crewjam's samlsp.Middleware, which
// owns its own session/cookie handling -- Sentry's session concept lives
// in internal/session, one layer up, so this package only does the SAML
// protocol mechanics (XML signing/parsing), per the explicit instruction
// not to hand-roll that crypto.
package saml

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/crewjam/saml"
)

type Config struct {
	// EntityID identifies Sentry to the IdP, conventionally Sentry's own
	// metadata URL.
	EntityID string
	// ACSURL is where the IdP redirects the browser back to with the
	// assertion (the "assertion consumer service" endpoint).
	ACSURL string
	// IDPMetadata is the IdP's metadata XML, fetched out-of-band (IdP
	// admin provides a URL or a file) and parsed by the caller via
	// samltypes/crewjam's metadata parsing -- kept out of this package's
	// constructor so it isn't doing its own network fetch of
	// admin-supplied, potentially untrusted URLs.
	IDPMetadata *saml.EntityDescriptor
	// Certificate/Key sign outgoing AuthnRequests and are required by
	// crewjam/saml's ServiceProvider even when the IdP doesn't mandate
	// signed requests. If nil, New generates a self-signed keypair --
	// fine for development, but a real deployment should supply a
	// certificate its IdP is configured to trust for encrypted
	// assertions, not rely on the generated one long-term.
	Certificate *tls.Certificate
}

type ServiceProvider struct {
	sp saml.ServiceProvider
}

func New(cfg Config) (*ServiceProvider, error) {
	if cfg.EntityID == "" || cfg.ACSURL == "" {
		return nil, fmt.Errorf("saml: EntityID and ACSURL are required")
	}
	if cfg.IDPMetadata == nil {
		return nil, fmt.Errorf("saml: IDPMetadata is required")
	}

	cert := cfg.Certificate
	if cert == nil {
		generated, err := selfSignedCert()
		if err != nil {
			return nil, fmt.Errorf("saml: generating a self-signed certificate: %w", err)
		}
		cert = generated
	}

	acsURL, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, fmt.Errorf("saml: parsing ACSURL: %w", err)
	}
	entityID, err := url.Parse(cfg.EntityID)
	if err != nil {
		return nil, fmt.Errorf("saml: parsing EntityID: %w", err)
	}

	return &ServiceProvider{
		sp: saml.ServiceProvider{
			Key:         cert.PrivateKey.(*rsa.PrivateKey),
			Certificate: parseLeaf(cert),
			MetadataURL: *entityID,
			AcsURL:      *acsURL,
			IDPMetadata: cfg.IDPMetadata,
		},
	}, nil
}

// LoginURL builds the redirect that starts SP-initiated SSO. relayState
// round-trips through the IdP and comes back with the response --
// typically where to send the browser after login completes, validated
// by the caller the same way OIDC's state parameter is (this package
// doesn't store it). Also returns the AuthnRequest's ID: the caller must
// persist it (e.g. a short-lived cookie, the same pattern
// enterprise/internal/loginhandler uses for OIDC's state) and pass it
// back via ParseResponse's possibleRequestIDs -- SAML's actual replay/
// unsolicited-response defense, standing in for OIDC's simpler state
// check. Skipping this (e.g. passing nil to ParseResponse) is exactly
// the "don't validate you asked for this response" mistake that would
// let an attacker replay a captured assertion.
func (s *ServiceProvider) LoginURL(relayState string) (redirectURL, requestID string, err error) {
	req, err := s.sp.MakeAuthenticationRequest(s.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", "", fmt.Errorf("saml: building authentication request: %w", err)
	}
	redirect, err := req.Redirect(relayState, &s.sp)
	if err != nil {
		return "", "", fmt.Errorf("saml: building redirect URL: %w", err)
	}
	return redirect.String(), req.ID, nil
}

// Claims is the subset of an assertion Sentry uses -- same "extend
// deliberately" reasoning as oidc.Claims.
type Claims struct {
	NameID string
	Email  string
}

// ParseResponse validates an incoming SAML response (signature, issuer,
// audience, timing) and extracts the fields Sentry cares about. This is
// the step that actually establishes trust -- crewjam/saml's
// ParseResponse does the XML signature verification, not this package.
func (s *ServiceProvider) ParseResponse(r *http.Request, possibleRequestIDs []string) (*Claims, error) {
	// crewjam/saml's ServiceProvider.ParseResponse reads req.PostForm
	// directly rather than parsing the body itself -- net/http only
	// populates PostForm once something calls ParseForm, which the
	// stdlib server never does on its own. Skipping this turns every
	// real ACS POST into an empty SAMLResponse (silently failing at the
	// base64-decode step), a bug this package's own real-fake-IdP test
	// caught immediately since it drives a genuine POST body.
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("saml: parsing ACS POST body: %w", err)
	}

	assertion, err := s.sp.ParseResponse(r, possibleRequestIDs)
	if err != nil {
		return nil, fmt.Errorf("saml: parsing/validating response: %w", err)
	}

	claims := &Claims{}
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		claims.NameID = assertion.Subject.NameID.Value
	}
	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			switch attr.Name {
			// "email" and the ADFS claims URI are what an IdP admin gets
			// by explicitly naming the attribute that way in the SAML
			// app's attribute-statement config. urn:oid:0.9.2342.19200300.100.1.3
			// is the standard LDAP "mail" OID -- what crewjam's own
			// DefaultAssertionMaker (and many real IdPs' default
			// templates) send when nothing more specific was requested,
			// found by tracing the actual assertion-building code this
			// test exercises rather than assuming "email" covers it.
			case "email", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", "urn:oid:0.9.2342.19200300.100.1.3":
				if len(attr.Values) > 0 {
					claims.Email = attr.Values[0].Value
				}
			}
		}
	}
	return claims, nil
}

func parseLeaf(cert *tls.Certificate) *x509.Certificate {
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// selfSignedCert generates a throwaway RSA keypair + certificate for
// development use, per Config.Certificate's doc comment.
func selfSignedCert() (*tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sentry-saml-sp-dev"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour * 365),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
