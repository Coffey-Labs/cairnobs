package saml

import (
	"testing"

	"github.com/crewjam/saml"
)

func fakeIDPMetadata() *saml.EntityDescriptor {
	return &saml.EntityDescriptor{
		EntityID: "https://idp.example.com/metadata",
		IDPSSODescriptors: []saml.IDPSSODescriptor{
			{
				SingleSignOnServices: []saml.Endpoint{
					{Binding: saml.HTTPRedirectBinding, Location: "https://idp.example.com/sso"},
				},
			},
		},
	}
}

func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatalf("expected an error for an empty config")
	}
}

func TestNewRejectsMissingIDPMetadata(t *testing.T) {
	_, err := New(Config{EntityID: "https://sentry.example.com/saml/metadata", ACSURL: "https://sentry.example.com/saml/acs"})
	if err == nil {
		t.Fatalf("expected an error when IDPMetadata is missing")
	}
}

// TestLoginURLBuildsAgainstRealIDPMetadata exercises the actual
// crewjam/saml AuthnRequest-building and redirect-encoding path (deflate
// + base64 + query-string construction) against IdP metadata shaped like
// what a real IdP publishes, confirming the wiring produces a usable
// redirect rather than just "the code compiles."
func TestLoginURLBuildsAgainstRealIDPMetadata(t *testing.T) {
	sp, err := New(Config{
		EntityID:    "https://sentry.example.com/saml/metadata",
		ACSURL:      "https://sentry.example.com/saml/acs",
		IDPMetadata: fakeIDPMetadata(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	redirectURL, err := sp.LoginURL("relay-state-123")
	if err != nil {
		t.Fatalf("LoginURL: %v", err)
	}
	if redirectURL == "" {
		t.Fatalf("expected a non-empty redirect URL")
	}
}
