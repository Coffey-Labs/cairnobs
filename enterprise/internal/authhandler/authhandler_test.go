package authhandler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sentry/sentry/enterprise/internal/session"
)

func testHandler(t *testing.T) (*Handler, *session.Manager) {
	t.Helper()
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{}), m
}

func doAuthorize(t *testing.T, h *Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorize", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAuthorizeViaServiceToken(t *testing.T) {
	h, m := testHandler(t)
	token, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Role != "service" || body.TenantID != "" || body.UserID != "" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestAuthorizeViaSessionCookie(t *testing.T) {
	h, m := testHandler(t)
	token, err := m.IssueUserSession("acme", "u1", "editor")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.TenantID != "acme" || body.UserID != "u1" || body.Role != "editor" {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestAuthorizeBearerTakesPrecedenceOverCookie(t *testing.T) {
	h, m := testHandler(t)
	serviceToken, err := m.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	sessionToken, err := m.IssueUserSession("acme", "u1", "viewer")
	if err != nil {
		t.Fatalf("IssueUserSession: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+serviceToken)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionToken})
	})
	var body authorizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Role != "service" {
		t.Fatalf("expected the Bearer service token to win, got role %q", body.Role)
	}
}

func TestAuthorizeNoCredentialsIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorize(t, h, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizeInvalidTokenIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-a-real-token")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestFeaturesReflectsConfiguredMechanisms(t *testing.T) {
	m, err := session.NewManager([]byte("this-is-a-32-byte-test-signing-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), m, Features{OIDCEnabled: true, SAMLEnabled: false})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/features", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body featuresResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !body.SSOConfigured || !body.OIDCEnabled || body.SAMLEnabled {
		t.Fatalf("unexpected features response: %+v", body)
	}
}

func TestFeaturesAllFalseWhenNothingConfigured(t *testing.T) {
	h, _ := testHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/features", nil))

	var body featuresResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.SSOConfigured || body.OIDCEnabled || body.SAMLEnabled {
		t.Fatalf("expected all-false features when nothing is configured, got %+v", body)
	}
}

func TestAuthorizeTokenFromWrongManagerIsUnauthorized(t *testing.T) {
	h, _ := testHandler(t)
	otherManager, err := session.NewManager([]byte("a-completely-different-32-byte-key!"))
	if err != nil {
		t.Fatalf("session.NewManager: %v", err)
	}
	token, err := otherManager.IssueServiceToken("alerting")
	if err != nil {
		t.Fatalf("IssueServiceToken: %v", err)
	}
	rec := doAuthorize(t, h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
