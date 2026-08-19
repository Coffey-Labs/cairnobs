// Package sessioncheck is alerting's half of local login (see
// api/localauth's package doc comment for the full feature). It only
// ever validates an already-issued session against the shared
// local_sessions table api/localauth writes to (same Postgres, no Go
// import) -- it never handles a raw password, never creates a session,
// and has no user-management surface at all; that stays exclusively in
// api. Deliberately its own small package rather than an import of
// api/localauth: this repo's hard, documented convention is no shared
// Go store/HTTP code between api and alerting, only /proto (see
// alerting/internal/httpserver/cors.go's WithCORS doc comment) --
// duplicating this one hash-and-look-up check is a small, low-risk
// price for keeping that boundary real.
package sessioncheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidSession = errors.New("sessioncheck: invalid or expired session")

type Checker struct {
	pool *pgxpool.Pool
}

func NewChecker(pool *pgxpool.Pool) *Checker {
	return &Checker{pool: pool}
}

// roleRank duplicates api/authz.Role's rank table -- same "no shared Go
// code between api and alerting" boundary this package's doc comment
// already explains for hashToken, applied to the one extra column
// (role) middleware.go now needs to enforce a floor on mutating
// requests (see RequireSession).
var roleRank = map[string]int{"viewer": 1, "editor": 2, "admin": 3, "owner": 4}

// roleSatisfies reports whether role meets minRole on the same
// Viewer<Editor<Admin<Owner scale api/authz.Role.Satisfies uses.
func roleSatisfies(role, minRole string) bool {
	return roleRank[role] >= roleRank[minRole]
}

// Validate hashes raw (plain SHA-256, no bcrypt -- see
// api/localauth/token.go's hashToken doc comment for why a session
// token doesn't need bcrypt's deliberate slowness) and checks it
// against local_sessions, returning the session's role snapshot
// alongside. Returns ErrInvalidSession for both "no such session" and
// "expired" -- middleware.go's caller doesn't distinguish them either,
// same posture api/localauth.Store.GetSession already takes for the
// same two cases.
func (c *Checker) Validate(ctx context.Context, raw string) (role string, err error) {
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])

	var expiresAt time.Time
	err = c.pool.QueryRow(ctx, `SELECT role, expires_at FROM local_sessions WHERE token_hash = $1`, hash).Scan(&role, &expiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidSession
		}
		return "", err
	}
	if expiresAt.Before(time.Now()) {
		return "", ErrInvalidSession
	}
	return role, nil
}
