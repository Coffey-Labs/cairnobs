// Package localauth is single-tenant mode's local username/password
// login: a real login page and session-based auth covering both /api
// and /alerting, plus a simple admin-managed user list, for deployments
// reachable over the internet that can no longer rely on Phase 0-3's
// "no auth yet" default (see /docs/architecture.md and CLAUDE.md's
// Phase 4 section for the enterprise/ SSO alternative this is not --
// this package has no tenant/RBAC-service concept, just "is this a
// valid logged-in user").
//
// Deliberately extends the existing users/tenants/tenant_memberships
// schema (0017/0018/0020_*.sql, built for Phase 4 SSO) rather than a
// parallel local_users table: tenant_memberships.role is already
// constrained to exactly authz.Role's four human values, so a local
// login gets real 4-tier roles for free, and a deployment that later
// turns on enterprise SSO has one identity graph to reconcile, not two.
// Every local user is a member of the "default" tenant only -- this
// package has no notion of provisioning additional tenants.
//
// Authorizer (authorizer.go) is what api/cmd/api/main.go wires into
// api/authz's Authorizer slot for a single-tenant deployment that wants
// real auth -- once that's non-nil, every existing RequireRole-wrapped
// route in dashboards/agents/queryapi/aiapi starts enforcing roles for
// free, no other handler file needs to change. alerting has no such
// per-route plumbing at all, so it gets its own, much smaller,
// deliberately-duplicated package (alerting/internal/sessioncheck) that
// only ever validates an already-issued session -- see that package's
// doc comment for why this isn't imported from here instead.
package localauth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/authz"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username already taken")
)

// defaultTenantID is the only tenant a local user can ever belong to --
// see the package doc comment. Matches every other single-tenant
// deployment's "default" tenant_id convention (dashboards, agents,
// alert_rules).
const defaultTenantID = "default"

type User struct {
	ID        string
	Username  string
	Role      authz.Role
	CreatedAt time.Time
}

type Session struct {
	UserID    string
	TenantID  string
	Role      authz.Role
	ExpiresAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateUser inserts a new local user and, in the same transaction, the
// tenant_memberships row that gives them role in the default tenant --
// a local user with no membership row would authenticate successfully
// (CreateSession has nothing that requires one) but satisfy no
// RequireRole check at all, so the two rows are never created
// separately.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, role authz.Role) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	id := uuid.NewString()
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO users (id, username, password_hash, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $2, now(), now())
		RETURNING created_at`,
		id, username, passwordHash).Scan(&createdAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, role)
		VALUES ($1, $2, $3, $4)`,
		uuid.NewString(), defaultTenantID, id, string(role)); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, Role: role, CreatedAt: createdAt}, nil
}

const listColumns = `
	u.id, u.username, tm.role, u.created_at`

// ListUsers only ever returns local users (username IS NOT NULL) --
// an SSO-provisioned user with no password_hash/username set never
// appears here, since there's nothing for this package's user manager
// to do with one.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+listColumns+`
		FROM users u
		JOIN tenant_memberships tm ON tm.user_id = u.id AND tm.tenant_id = $1
		WHERE u.username IS NOT NULL
		ORDER BY u.username`, defaultTenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		var role string
		if err := rows.Scan(&u.ID, &u.Username, &role, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Role = authz.Role(role)
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserForLogin returns the user and their password hash together --
// the only place this package ever reads a password_hash back out, and
// only to feed ComparePassword. Everywhere else uses User, which never
// carries the hash.
func (s *Store) GetUserForLogin(ctx context.Context, username string) (*User, string, error) {
	var u User
	var role, hash string
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.password_hash, tm.role, u.created_at
		FROM users u
		JOIN tenant_memberships tm ON tm.user_id = u.id AND tm.tenant_id = $1
		WHERE u.username = $2`, defaultTenantID, username).
		Scan(&u.ID, &u.Username, &hash, &role, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	u.Role = authz.Role(role)
	return &u, hash, nil
}

// GetUserByID backs GET /auth/session -- looking up the identity
// RequireRole already resolved and attached to the request context, to
// return its username (Session/Identity carry no username, only IDs).
func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	var u User
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, tm.role, u.created_at
		FROM users u
		JOIN tenant_memberships tm ON tm.user_id = u.id AND tm.tenant_id = $1
		WHERE u.id = $2 AND u.username IS NOT NULL`, defaultTenantID, id).
		Scan(&u.ID, &u.Username, &role, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Role = authz.Role(role)
	return &u, nil
}

// DeleteUser cascades to the user's tenant_memberships and
// local_sessions rows (both ON DELETE CASCADE) -- a deleted user's
// existing sessions stop validating immediately, not just their next
// login.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1 AND username IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPasswordHash also revokes every existing session for userID, in
// the same transaction -- Session.Role/TenantID are a snapshot taken at
// login (see 0041_create_local_sessions.sql's doc comment), so without
// this an account whose password was just reset for security reasons
// would keep any already-issued session working regardless.
func (s *Store) SetPasswordHash(ctx context.Context, userID, hash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $1, updated_at = now() WHERE id = $2 AND username IS NOT NULL`, hash, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM local_sessions WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetRole also revokes every existing session for userID, in the same
// transaction -- Session.Role is a snapshot taken at login (see
// SetPasswordHash's doc comment above for why), so without this a
// demoted user would keep acting under their old, higher-privileged
// role for the rest of an already-issued session's lifetime.
func (s *Store) SetRole(ctx context.Context, userID string, role authz.Role) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE tenant_memberships SET role = $1
		WHERE user_id = $2 AND tenant_id = $3
		AND EXISTS (SELECT 1 FROM users WHERE id = $2 AND username IS NOT NULL)`,
		string(role), userID, defaultTenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM local_sessions WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CountLocalUsers backs -seed-admin's idempotency check (see
// cmd/api/main.go's runSeedAdmin): a deployment that already has at
// least one local user never gets a second auto-created admin account.
func (s *Store) CountLocalUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username IS NOT NULL`).Scan(&n)
	return n, err
}

// CreateSession mints a fresh opaque token for an already-authenticated
// user (login has already verified their password by the time this is
// called) and stores its hash plus a role/tenant snapshot. Returns the
// raw token -- the only time it's ever available in plaintext again
// after this call.
func (s *Store) CreateSession(ctx context.Context, userID, tenantID string, role authz.Role, ttl time.Duration) (string, error) {
	raw, hash, err := newOpaqueToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO local_sessions (id, user_id, tenant_id, role, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), userID, tenantID, string(role), hash, time.Now().Add(ttl))
	if err != nil {
		return "", err
	}
	return raw, nil
}

// GetSession looks up an already-hashed lookup key rather than a raw
// token -- see authorizer.go, the only caller, which re-derives the
// hash from whatever the request presented before calling this.
// Deliberately does not delete an expired row itself (that's a plain
// SELECT with no side effect); the goal here is a fast, obviously-
// correct read path, not a lookup that also mutates state, so
// expired-session cleanup is a separate, simpler concern.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	var sess Session
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, tenant_id, role, expires_at
		FROM local_sessions WHERE token_hash = $1`, tokenHash).
		Scan(&sess.UserID, &sess.TenantID, &role, &sess.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sess.Role = authz.Role(role)
	if sess.ExpiresAt.Before(time.Now()) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteSessionByHash backs logout -- a no-op (not an error) if the
// session is already gone, matching logout's own "always succeeds"
// posture (handler.go's handleLogout).
func (s *Store) DeleteSessionByHash(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM local_sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// isUniqueViolation checks for Postgres error code 23505 (unique_violation),
// same pgconn.PgError.Code pattern rbacstore.go's SetDataSourceCredentials
// already uses for 22P02.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
