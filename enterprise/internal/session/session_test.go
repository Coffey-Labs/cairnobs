package session

import (
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
)

func testKey() []byte {
	return []byte("this-is-a-32-byte-test-signing-key!")
}

func TestNewManagerRejectsShortKey(t *testing.T) {
	if _, err := NewManager([]byte("too-short")); err == nil {
		t.Fatal("expected an error for a signing key under 32 bytes")
	}
}

func TestIssueAndValidateUserSession(t *testing.T) {
	m, err := NewManager(testKey())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	token, err := m.IssueUserSession("acme", "u1", "editor")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	claims, err := m.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.TenantID != "acme" || claims.UserID != "u1" || claims.Role != "editor" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestIssueAndValidateServiceToken(t *testing.T) {
	m, err := NewManager(testKey())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	token, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	claims, err := m.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Role != "service" || claims.Subject != "alerting" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.TenantID != "" || claims.UserID != "" {
		t.Fatalf("service token must not carry a tenant/user -- tenant is resolved server-side per request, got %+v", claims)
	}
}

func TestValidateRejectsTamperedToken(t *testing.T) {
	m, err := NewManager(testKey())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	token, err := m.IssueUserSession("acme", "u1", "viewer")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	// Flip a character in the payload segment to simulate tampering.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-segment JWT, got %d segments", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "x" + "." + parts[2]
	if _, err := m.Validate(tampered); err != ErrInvalidToken {
		t.Fatalf("Validate(tampered) error = %v, want ErrInvalidToken", err)
	}
}

func TestValidateRejectsWrongKey(t *testing.T) {
	m1, err := NewManager(testKey())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m2, err := NewManager([]byte("a-completely-different-32-byte-key!"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	token, err := m1.IssueUserSession("acme", "u1", "viewer")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	if _, err := m2.Validate(token); err != ErrInvalidToken {
		t.Fatalf("Validate with wrong key error = %v, want ErrInvalidToken", err)
	}
}

func TestValidateRejectsExpiredToken(t *testing.T) {
	m, err := NewManager(testKey())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	now := time.Now()
	claims := Claims{
		TenantID: "acme", UserID: "u1", Role: "viewer",
		Claims: jwt.Claims{
			IssuedAt: jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			Expiry:   jwt.NewNumericDate(now.Add(-1 * time.Hour)),
		},
	}
	token, err := jwt.Signed(m.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("building an already-expired token: %v", err)
	}
	if _, err := m.Validate(token); err != ErrInvalidToken {
		t.Fatalf("Validate(expired) error = %v, want ErrInvalidToken", err)
	}
}
