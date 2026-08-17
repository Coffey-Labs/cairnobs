// Package audit implements the append-only, hash-chained query audit
// log described in /docs/phase-4-isolation-design.md's audit-logging
// section. Two independent defenses back the "no update/delete path
// from the application layer" requirement -- both verified against a
// live Postgres, not just written: audit_writer (this package's own
// Postgres role, via its own connection pool, never the shared `sentry`
// role every other store uses) has only INSERT+SELECT grants, and a
// BEFORE UPDATE OR DELETE trigger (metadata/migrations/0015-0016)
// rejects the operation for *any* role, including the table owner --
// confirmed live: even `sentry` cannot UPDATE a row without first
// disabling the trigger, a privileged operation distinct from ordinary
// application access.
//
// The hash chain (prev_hash/row_hash) proves internal consistency --
// detects tampering with existing rows -- but does not by itself prove
// truth against a privileged attacker who can rewrite the whole table
// and regenerate a self-consistent chain from row 1. See checkpoint.go
// for the external-anchoring half of that guarantee.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Source string

const (
	SourceAPI      Source = "api"
	SourceWeb      Source = "web"
	SourceCLI      Source = "cli"
	SourceAlerting Source = "alerting"
)

type EventType string

const (
	EventQuery           EventType = "query"
	EventRoleChange      EventType = "role_change"
	EventGrantChange     EventType = "grant_change"
	EventSSOConfigChange EventType = "sso_config_change"
	EventSecretReveal    EventType = "secret_reveal"
	// EventAIInteraction (Phase 7 task 12): a translate/fix/optimize
	// suggestion's accept-or-dismiss outcome. QueryText carries the
	// resulting query (if any); Detail carries the operation, the
	// original input, confidence, and whether the user edited the
	// suggestion before using it -- see ai_interaction_adapter.go.
	EventAIInteraction EventType = "ai_interaction"
)

type Status string

const (
	StatusSuccess Status = "success"
	StatusError   Status = "error"
)

// Entry is what a caller supplies. UserID is nil for system/alerting-
// sourced entries (see /docs/phase-4-isolation-design.md's alerting
// service-identity finding -- alerting evaluations are audited, but
// aren't attributable to a human user).
type Entry struct {
	TenantID     string
	UserID       *string
	Source       Source
	EventType    EventType
	QueryText    *string
	RowCount     *int
	DurationMS   *int
	Status       Status
	ErrorMessage *string
	Detail       json.RawMessage
}

// Record is a written entry plus the fields the store assigned.
type Record struct {
	Entry
	ID       int64
	PrevHash *string
	RowHash  string
}

// Store writes via a connection pool authenticated as the audit_writer
// role -- never the shared pool other stores in this repo use. Passing
// a pool opened with any other role's credentials silently defeats the
// grant-restriction half of this package's guarantee; there's no way
// for this package to verify its own pool's role at runtime, so this is
// an integration-time discipline documented here, not something this
// code can enforce on itself.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// advisoryLockKey serializes concurrent Append calls so two writers
// never read the same prev_hash and each compute a hash chained off it
// -- that would fork the chain. Arbitrary fixed value, held only for
// the duration of one transaction (pg_advisory_xact_lock releases
// automatically at commit/rollback).
const advisoryLockKey = 784129035

func (s *Store) Append(ctx context.Context, e Entry) (*Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit: beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockKey); err != nil {
		return nil, fmt.Errorf("audit: acquiring serialization lock: %w", err)
	}

	var prevHash *string
	row := tx.QueryRow(ctx, "SELECT row_hash FROM audit_log ORDER BY id DESC LIMIT 1")
	if err := row.Scan(&prevHash); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("audit: reading previous row hash: %w", err)
	}

	if len(e.Detail) == 0 {
		e.Detail = json.RawMessage(`{}`)
	}
	rec := &Record{Entry: e, PrevHash: prevHash, RowHash: computeHash(prevHash, e)}

	err = tx.QueryRow(ctx, `
		INSERT INTO audit_log (tenant_id, user_id, source, event_type, query_text, row_count,
		                        duration_ms, status, error_message, detail, prev_hash, row_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		e.TenantID, e.UserID, e.Source, e.EventType, e.QueryText, e.RowCount,
		e.DurationMS, e.Status, e.ErrorMessage, e.Detail, prevHash, rec.RowHash,
	).Scan(&rec.ID)
	if err != nil {
		return nil, fmt.Errorf("audit: inserting row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("audit: committing: %w", err)
	}
	return rec, nil
}

// computeHash is deliberately a fixed, explicit field order (not "hash
// the JSON encoding," which is not guaranteed stable across Go versions
// or map key ordering) -- \x00 is used as a field separator since it
// cannot appear in any of these string fields in practice, and even if
// it somehow did, the goal here is deterministic tamper-detection
// against accidental/naive modification, not cryptographic
// collision-resistance against a chosen-plaintext adversary.
func computeHash(prevHash *string, e Entry) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(deref(prevHash))
	write(e.TenantID)
	write(deref(e.UserID))
	write(string(e.Source))
	write(string(e.EventType))
	write(deref(e.QueryText))
	write(intToStr(e.RowCount))
	write(intToStr(e.DurationMS))
	write(string(e.Status))
	write(deref(e.ErrorMessage))
	write(string(e.Detail))
	return hex.EncodeToString(h.Sum(nil))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intToStr(n *int) string {
	if n == nil {
		return ""
	}
	return fmt.Sprintf("%d", *n)
}
