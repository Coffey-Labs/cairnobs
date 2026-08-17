// Package agentregistry is the Postgres-backed implementation of
// grpcserver.AgentRegistry -- ingest's half of agent inventory/remote
// config (see /docs/agent-management-design.md). Writes into the same
// sentry_metadata Postgres api reads/writes from for the web UI's
// inventory and edit-config views (api/agents), the same shared-schema-
// different-services shape alerting and api already use for dashboards/
// alert_rules.
package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/ingest/internal/grpcserver"
)

// defaultTenantID is used when no TenantResolver is configured (empty
// tenantID from grpcserver) -- the same 'default' tenant row every
// single-tenant deployment's Postgres already has, seeded by
// metadata/migrations/0019_seed_default_tenant.sql.
const defaultTenantID = "default"

// overrideFields is the JSON shape stored in agents.desired_override.
// Deliberately duplicated in api/agents rather than imported -- these
// are two different Go modules, and this codebase's established
// convention (see enterprise/internal/apiconfig.AIConfig,
// grpcserver.TenantIDHeaderKey) is to duplicate a small shared shape
// across a module boundary rather than couple two independently
// deployable services' builds together. Keep the two in sync by hand.
type overrideFields struct {
	BatchMaxSize         *uint64 `json:"batch_max_size,omitempty"`
	BatchFlushIntervalMS *uint64 `json:"batch_flush_interval_ms,omitempty"`
	HeartbeatEnabled     *bool   `json:"heartbeat_enabled,omitempty"`
	HeartbeatIntervalMS  *uint64 `json:"heartbeat_interval_ms,omitempty"`
	JournaldUnit         *string `json:"journald_unit,omitempty"`
}

type Registry struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Registry {
	return &Registry{pool: pool}
}

var _ grpcserver.AgentRegistry = (*Registry)(nil)

func (r *Registry) CheckIn(ctx context.Context, tenantID string, info grpcserver.AgentCheckIn) (grpcserver.CheckInResult, error) {
	if tenantID == "" {
		tenantID = defaultTenantID
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return grpcserver.CheckInResult{}, fmt.Errorf("agentregistry: beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Read (and lock) whatever pending_command exists BEFORE the
	// upsert below clears it -- as two real, ordered statements in one
	// transaction, not one clever statement. An earlier version tried
	// to do this with a single INSERT...ON CONFLICT plus a sibling CTE
	// referenced only from RETURNING, on the assumption that Postgres
	// evaluates every part of a WITH query against the same pre-
	// statement snapshot; that assumption is wrong specifically for
	// FOR UPDATE, which always locks (and therefore reads) the latest
	// row version to do its job, including versions written earlier in
	// the SAME statement -- confirmed empirically against a live
	// Postgres (the CTE's FOR UPDATE was reading its own sibling
	// UPDATE's just-cleared NULL, always reporting "no command" even
	// when one was genuinely pending). Two statements in one
	// transaction has no such ambiguity: the SELECT strictly
	// happens-before the UPDATE, full stop. FOR UPDATE against zero
	// rows (a brand-new agent's first-ever check-in) is a harmless
	// no-op -- there's nothing to lock or have a pending command yet.
	var pendingCommand *string
	err = tx.QueryRow(ctx, `SELECT pending_command FROM agents WHERE tenant_id = $1 AND host = $2 FOR UPDATE`,
		tenantID, info.Host,
	).Scan(&pendingCommand)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return grpcserver.CheckInResult{}, fmt.Errorf("agentregistry: reading pending command: %w", err)
	}

	var (
		desiredOverride []byte
		desiredVersion  *string
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO agents (
			id, tenant_id, host, service,
			reported_agent_version, reported_source_kind, reported_source_detail,
			reported_batch_max_size, reported_batch_flush_ms,
			reported_heartbeat_on, reported_heartbeat_ms,
			first_seen_at, last_seen_at, applied_override_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now(), now(), $12)
		ON CONFLICT (tenant_id, host) DO UPDATE SET
			service                  = EXCLUDED.service,
			reported_agent_version   = EXCLUDED.reported_agent_version,
			reported_source_kind     = EXCLUDED.reported_source_kind,
			reported_source_detail   = EXCLUDED.reported_source_detail,
			reported_batch_max_size  = EXCLUDED.reported_batch_max_size,
			reported_batch_flush_ms  = EXCLUDED.reported_batch_flush_ms,
			reported_heartbeat_on    = EXCLUDED.reported_heartbeat_on,
			reported_heartbeat_ms    = EXCLUDED.reported_heartbeat_ms,
			last_seen_at             = now(),
			applied_override_version = EXCLUDED.applied_override_version,
			pending_command          = NULL
		RETURNING desired_override, desired_override_version`,
		uuid.NewString(), tenantID, info.Host, info.Service,
		info.AgentVersion, info.SourceKind, info.SourceDetail,
		info.BatchMaxSize, info.BatchFlushIntervalMS,
		info.HeartbeatEnabled, info.HeartbeatIntervalMS,
		info.AppliedOverrideVersion,
	).Scan(&desiredOverride, &desiredVersion)
	if err != nil {
		return grpcserver.CheckInResult{}, fmt.Errorf("agentregistry: upserting check-in: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return grpcserver.CheckInResult{}, fmt.Errorf("agentregistry: committing check-in: %w", err)
	}

	result := grpcserver.CheckInResult{}
	if pendingCommand != nil {
		result.Command = *pendingCommand
	}

	if desiredVersion == nil || len(desiredOverride) == 0 {
		return result, nil
	}

	var fields overrideFields
	if err := json.Unmarshal(desiredOverride, &fields); err != nil {
		return grpcserver.CheckInResult{}, fmt.Errorf("agentregistry: parsing stored override: %w", err)
	}
	result.Override = grpcserver.AgentOverride{
		HasOverride:          true,
		BatchMaxSize:         fields.BatchMaxSize,
		BatchFlushIntervalMS: fields.BatchFlushIntervalMS,
		HeartbeatEnabled:     fields.HeartbeatEnabled,
		HeartbeatIntervalMS:  fields.HeartbeatIntervalMS,
		JournaldUnit:         fields.JournaldUnit,
		Version:              *desiredVersion,
	}
	return result, nil
}
