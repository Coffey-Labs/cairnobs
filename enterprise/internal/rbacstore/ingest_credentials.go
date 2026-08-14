package rbacstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IngestCredential is one ingest_credentials row -- see
// metadata/migrations/0034_create_ingest_credentials.sql's doc comment
// for why only a hash is stored. Consumed by
// ingest/internal/grpcserver.TenantResolver (an HTTP call to
// enterprise-auth's POST /internal/authorize-ingest, which calls
// ValidateIngestCredential below) so an agent's records can be
// attributed to a tenant at the point they enter the system.
type IngestCredential struct {
	ID        string
	TenantID  string
	CreatedAt time.Time
}

func hashIngestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateIngestCredential generates a new bearer token for tenantID and
// returns the plaintext exactly once -- only its hash is ever persisted
// (see this file's package doc comment). There is no way to retrieve a
// lost token again; the only recovery is issuing a new one
// (RevokeIngestCredential + CreateIngestCredential), the same "can't
// recover, can only reissue" UX every real API-key system uses.
func (s *Store) CreateIngestCredential(ctx context.Context, tenantID string) (token string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("rbacstore: generating ingest credential: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO ingest_credentials (id, tenant_id, token_hash)
		VALUES ($1, $2, $3)`,
		uuid.NewString(), tenantID, hashIngestToken(token))
	if err != nil {
		return "", fmt.Errorf("rbacstore: creating ingest credential: %w", err)
	}
	return token, nil
}

// ValidateIngestCredential hashes the presented token and looks up which
// tenant it belongs to via an indexed exact-match on the UNIQUE
// token_hash column -- the only production call site is
// enterprise-auth's POST /internal/authorize-ingest handler, per an
// agent's PushBatch request.
func (s *Store) ValidateIngestCredential(ctx context.Context, token string) (tenantID string, err error) {
	row := s.pool.QueryRow(ctx, `SELECT tenant_id FROM ingest_credentials WHERE token_hash = $1`, hashIngestToken(token))
	if err := row.Scan(&tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("rbacstore: validating ingest credential: %w", err)
	}
	return tenantID, nil
}

// RevokeIngestCredential deletes a credential by ID (not by token --
// the plaintext is never stored, so revocation has to name the row some
// other way; ListIngestCredentialsForTenant is what an operator uses to
// find the ID).
func (s *Store) RevokeIngestCredential(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ingest_credentials WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("rbacstore: revoking ingest credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListIngestCredentialsForTenant never returns the plaintext token (it
// isn't stored) -- just enough (ID, creation time) for an operator to
// decide which one to revoke.
func (s *Store) ListIngestCredentialsForTenant(ctx context.Context, tenantID string) ([]IngestCredential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, created_at FROM ingest_credentials
		WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("rbacstore: listing ingest credentials: %w", err)
	}
	defer rows.Close()

	var out []IngestCredential
	for rows.Next() {
		var c IngestCredential
		if err := rows.Scan(&c.ID, &c.TenantID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("rbacstore: scanning ingest credential: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
