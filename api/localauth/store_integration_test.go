// Exercises the actual parameterized SQL in store.go against a real
// Postgres -- handler_test.go's fakeStore is hand-written to mimic this
// SQL's behavior, but can't catch a real gap like a typo in a WHERE
// clause, a wrong column name, or (the specific thing worth testing
// here) whether 0040/0041's schema/FK/CHECK constraints actually hold
// the shape this package assumes. Same "skip unless a live-Postgres env
// var is set" convention as api/dashboards/store_integration_test.go.
//
// Skipped unless LOCALAUTH_TEST_POSTGRES_ADDR is set; run via:
//
//	docker run --rm --network sentry_default -v $(pwd)/../../..:/src -w /src/api \
//	  -e LOCALAUTH_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
//	  -e LOCALAUTH_TEST_POSTGRES_PASSWORD=sentry-dev-only \
//	  golang:1.25-alpine go test ./localauth/... -run Integration -v
package localauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/authz"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("LOCALAUTH_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("LOCALAUTH_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("LOCALAUTH_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://sentry:%s@%s/sentry_metadata", password, addr)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

func testUsername(t *testing.T) string {
	t.Helper()
	return "test-" + uuid.NewString()[:8]
}

func TestIntegrationCreateAndLoginUser(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	created, err := store.CreateUser(ctx, username, hash, authz.RoleEditor)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, created.ID) })

	got, gotHash, err := store.GetUserForLogin(ctx, username)
	if err != nil {
		t.Fatalf("GetUserForLogin: %v", err)
	}
	if got.ID != created.ID || got.Role != authz.RoleEditor {
		t.Errorf("GetUserForLogin = %+v, want id=%s role=editor", got, created.ID)
	}
	if !ComparePassword(gotHash, "correct horse battery staple") {
		t.Errorf("stored hash does not verify against the original password")
	}
}

func TestIntegrationDuplicateUsernameRejected(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	created, err := store.CreateUser(ctx, username, hash, authz.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, created.ID) })

	if _, err := store.CreateUser(ctx, username, hash, authz.RoleViewer); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("second CreateUser with the same username: err = %v, want ErrUsernameTaken", err)
	}
}

func TestIntegrationSessionRoundTripAndExpiry(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	raw, err := store.CreateSession(ctx, user.ID, "default", authz.RoleAdmin, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := store.GetSession(ctx, hashToken(raw))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.UserID != user.ID || sess.Role != authz.RoleAdmin {
		t.Errorf("GetSession = %+v, want user_id=%s role=admin", sess, user.ID)
	}

	// An already-expired session (negative TTL) must not validate --
	// exercises the real expires_at comparison against Postgres's own
	// now(), not just Go's clock.
	expiredRaw, err := store.CreateSession(ctx, user.ID, "default", authz.RoleAdmin, -time.Hour)
	if err != nil {
		t.Fatalf("CreateSession (expired): %v", err)
	}
	if _, err := store.GetSession(ctx, hashToken(expiredRaw)); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession on an expired session: err = %v, want ErrNotFound", err)
	}
}

func TestIntegrationSetPasswordHashRevokesSessions(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	raw, err := store.CreateSession(ctx, user.ID, "default", authz.RoleViewer, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	newHash, _ := HashPassword("a-new-password")
	if err := store.SetPasswordHash(ctx, user.ID, newHash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	if _, err := store.GetSession(ctx, hashToken(raw)); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession after password reset: err = %v, want ErrNotFound (reset must revoke existing sessions)", err)
	}
}
