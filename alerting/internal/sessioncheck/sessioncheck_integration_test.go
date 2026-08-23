// Exercises Checker.Validate against a real local_sessions row --
// unlike a fake, this confirms alerting can actually read the rows
// api/localauth (a separate Go module/service) writes into the shared
// Postgres, including the exact hash function agreeing on both sides.
// Same "skip unless a live-Postgres env var is set" convention as
// api/dashboards/store_integration_test.go.
//
// Skipped unless SESSIONCHECK_TEST_POSTGRES_ADDR is set; run via:
//
//	docker run --rm --network cairnobs_default -v $(pwd)/../../..:/src -w /src/alerting \
//	  -e SESSIONCHECK_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e SESSIONCHECK_TEST_POSTGRES_PASSWORD=cairnobs-dev-only \
//	  golang:1.25-alpine go test ./internal/sessioncheck/... -run Integration -v
package sessioncheck

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	addr := os.Getenv("SESSIONCHECK_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("SESSIONCHECK_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("SESSIONCHECK_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://cairnobs:%s@%s/cairnobs_metadata", password, addr)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// insertTestSession writes directly into local_sessions and users --
// this package has no Store type of its own (see package doc comment:
// creating a session is api/localauth's job, this only ever validates
// one), so a real row has to come from somewhere for the test to check
// against.
func insertTestSession(t *testing.T, pool *pgxpool.Pool, ttl time.Duration) (raw string) {
	t.Helper()
	return insertTestSessionWithRole(t, pool, ttl, "viewer")
}

func insertTestSessionWithRole(t *testing.T, pool *pgxpool.Pool, ttl time.Duration, role string) (raw string) {
	t.Helper()
	ctx := context.Background()

	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, password_hash, display_name, created_at, updated_at)
		VALUES ($1, $2, 'unused', $2, now(), now())`,
		userID, "test-"+userID[:8]); err != nil {
		t.Fatalf("inserting test user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })

	buf := make([]byte, 32)
	sum := sha256.Sum256([]byte(userID + role)) // deterministic-enough per-test randomness without crypto/rand here
	copy(buf, sum[:])
	raw = base64.RawURLEncoding.EncodeToString(buf)
	hashSum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(hashSum[:])

	if _, err := pool.Exec(ctx, `
		INSERT INTO local_sessions (id, user_id, tenant_id, role, token_hash, expires_at)
		VALUES ($1, $2, 'default', $3, $4, $5)`,
		uuid.NewString(), userID, role, hash, time.Now().Add(ttl)); err != nil {
		t.Fatalf("inserting test session: %v", err)
	}
	return raw
}

func TestIntegrationValidateAcceptsRealSession(t *testing.T) {
	pool := integrationPool(t)
	raw := insertTestSession(t, pool, time.Hour)

	role, err := NewChecker(pool).Validate(context.Background(), raw)
	if err != nil {
		t.Errorf("Validate on a real, unexpired session: err = %v, want nil", err)
	}
	if role != "viewer" {
		t.Errorf("role = %q, want %q (matches insertTestSession's role column)", role, "viewer")
	}
}

func TestIntegrationValidateRejectsExpiredSession(t *testing.T) {
	pool := integrationPool(t)
	raw := insertTestSession(t, pool, -time.Hour)

	if _, err := NewChecker(pool).Validate(context.Background(), raw); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("Validate on an expired session: err = %v, want ErrInvalidSession", err)
	}
}

func TestIntegrationValidateRejectsUnknownToken(t *testing.T) {
	pool := integrationPool(t)

	if _, err := NewChecker(pool).Validate(context.Background(), "not-a-real-token"); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("Validate on an unknown token: err = %v, want ErrInvalidSession", err)
	}
}

// TestIntegrationRequireSessionForbidsMutatingRequestFromViewer is the
// regression test for the security-audit finding that this middleware
// used to be a pure "logged in or not" gate: a Viewer-role session
// could create/delete alert rules and notification targets exactly like
// an Editor. A POST from a Viewer session must now be 403, not passed
// through to the handler.
func TestIntegrationRequireSessionForbidsMutatingRequestFromViewer(t *testing.T) {
	pool := integrationPool(t)
	raw := insertTestSessionWithRole(t, pool, time.Hour, "viewer")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) })
	handler := RequireSession(NewChecker(pool), next)

	req := httptest.NewRequest(http.MethodPost, "/targets", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Error("handler must not run for a Viewer's mutating request")
	}
}

// TestIntegrationRequireSessionAllowsMutatingRequestFromEditor is the
// positive counterpart: an Editor-role session (the new floor) must
// still be able to reach mutating routes.
func TestIntegrationRequireSessionAllowsMutatingRequestFromEditor(t *testing.T) {
	pool := integrationPool(t)
	raw := insertTestSessionWithRole(t, pool, time.Hour, "editor")

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireSession(NewChecker(pool), next)

	req := httptest.NewRequest(http.MethodPost, "/targets", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestIntegrationRequireSessionAllowsReadFromViewer confirms the read
// path is untouched: GET still only needs a valid session, any role.
func TestIntegrationRequireSessionAllowsReadFromViewer(t *testing.T) {
	pool := integrationPool(t)
	raw := insertTestSessionWithRole(t, pool, time.Hour, "viewer")

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireSession(NewChecker(pool), next)

	req := httptest.NewRequest(http.MethodGet, "/targets", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
