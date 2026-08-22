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
//	  -e LOCALAUTH_TEST_POSTGRES_PASSWORD=cairnobs-dev-only \
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

	"github.com/cairnobs/cairnobs/api/authz"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("LOCALAUTH_TEST_POSTGRES_ADDR")
	if addr == "" {
		t.Skip("LOCALAUTH_TEST_POSTGRES_ADDR not set -- skipping live-Postgres integration test")
	}
	password := os.Getenv("LOCALAUTH_TEST_POSTGRES_PASSWORD")
	dsn := fmt.Sprintf("postgres://cairnobs:%s@%s/cairnobs_metadata", password, addr)
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

func TestIntegrationSetRoleRevokesSessions(t *testing.T) {
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

	if err := store.SetRole(ctx, user.ID, authz.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	got, _, err := store.GetUserForLogin(ctx, username)
	if err != nil {
		t.Fatalf("GetUserForLogin: %v", err)
	}
	if got.Role != authz.RoleAdmin {
		t.Errorf("role after SetRole = %q, want admin", got.Role)
	}

	if _, err := store.GetSession(ctx, hashToken(raw)); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession after role change: err = %v, want ErrNotFound (role change must revoke existing sessions)", err)
	}
}

// TestIntegrationSetRoleAcceptsEveryRole confirms tenant_memberships'
// role CHECK constraint (0020_create_tenant_memberships.sql) accepts
// all four roles via SetRole's UPDATE, not just CreateUser's INSERT --
// the handler-level fake-store test already covers all sixteen ordered
// transitions, but only a real Postgres run proves the constraint
// itself doesn't reject any of them.
func TestIntegrationSetRoleAcceptsEveryRole(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	for _, role := range []authz.Role{authz.RoleEditor, authz.RoleAdmin, authz.RoleOwner, authz.RoleViewer} {
		if err := store.SetRole(ctx, user.ID, role); err != nil {
			t.Fatalf("SetRole(%s): %v", role, err)
		}
		got, _, err := store.GetUserForLogin(ctx, username)
		if err != nil {
			t.Fatalf("GetUserForLogin after SetRole(%s): %v", role, err)
		}
		if got.Role != role {
			t.Fatalf("role after SetRole(%s) = %q, want %q", role, got.Role, role)
		}
	}
}

// TestIntegrationCountUsersWithRole confirms the count reflects real
// inserts/role changes against the actual tenant_memberships table --
// the "at least one owner" guard this backs (handler.go's
// wouldRemoveLastOwner) is only as trustworthy as this query.
func TestIntegrationCountUsersWithRole(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleOwner)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	before, err := store.CountUsersWithRole(ctx, authz.RoleOwner)
	if err != nil {
		t.Fatalf("CountUsersWithRole(owner): %v", err)
	}
	if before < 1 {
		t.Fatalf("owner count = %d, want at least 1 (the user just created)", before)
	}

	if err := store.SetRole(ctx, user.ID, authz.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	after, err := store.CountUsersWithRole(ctx, authz.RoleOwner)
	if err != nil {
		t.Fatalf("CountUsersWithRole(owner) after demotion: %v", err)
	}
	if after != before-1 {
		t.Fatalf("owner count after demoting one = %d, want %d", after, before-1)
	}
}

// TestIntegrationGetPasswordHashByID confirms it reads the same hash
// GetUserForLogin does, by ID instead of username -- the self-service
// password change handler's only way to fetch the caller's own hash.
func TestIntegrationGetPasswordHashByID(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("correct horse battery staple")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	got, err := store.GetPasswordHashByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetPasswordHashByID: %v", err)
	}
	if !ComparePassword(got, "correct horse battery staple") {
		t.Errorf("hash from GetPasswordHashByID does not verify against the original password")
	}

	if _, err := store.GetPasswordHashByID(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPasswordHashByID for an unknown ID: err = %v, want ErrNotFound", err)
	}
}

// TestIntegrationDisplayTimezoneDefaultsAndPersists is the half
// handler_test.go's fake can't prove: that migration 0042's column
// actually exists with its NOT NULL DEFAULT 'UTC', that GetUserByID's
// SELECT names it correctly, and that a session created before the
// change keeps working after it (the UPDATE deliberately doesn't touch
// local_sessions, unlike SetPasswordHash/SetRole).
func TestIntegrationDisplayTimezoneDefaultsAndPersists(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	username := testUsername(t)

	hash, _ := HashPassword("password1")
	user, err := store.CreateUser(ctx, username, hash, authz.RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteUser(ctx, user.ID) })

	fetched, err := store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if fetched.DisplayTimezone != "UTC" {
		t.Fatalf("new user DisplayTimezone = %q, want %q (schema default)", fetched.DisplayTimezone, "UTC")
	}

	raw, err := store.CreateSession(ctx, user.ID, "default", authz.RoleViewer, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.SetDisplayTimezone(ctx, user.ID, "Australia/Adelaide"); err != nil {
		t.Fatalf("SetDisplayTimezone: %v", err)
	}
	fetched, err = store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID after set: %v", err)
	}
	if fetched.DisplayTimezone != "Australia/Adelaide" {
		t.Errorf("DisplayTimezone = %q, want %q", fetched.DisplayTimezone, "Australia/Adelaide")
	}

	if _, err := store.GetSession(ctx, hashToken(raw)); err != nil {
		t.Errorf("session after timezone change: %v, want it to still be valid", err)
	}
}

func TestIntegrationSetDisplayTimezoneUnknownUser(t *testing.T) {
	store := integrationStore(t)
	if err := store.SetDisplayTimezone(context.Background(), uuid.NewString(), "UTC"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDisplayTimezone on missing user = %v, want ErrNotFound", err)
	}
}
