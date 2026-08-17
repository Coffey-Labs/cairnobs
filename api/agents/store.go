// Package agents is the web-facing half of agent inventory/remote
// config (see /docs/agent-management-design.md) -- reads/writes the
// same `agents` table ingest's internal/agentregistry writes on every
// CheckIn RPC, the same shared-schema-different-services shape
// alerting and api already use for dashboards/alert_rules.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// CommandRestart is the one supported lifecycle command -- see
// ingest/internal/grpcserver.AgentCommandRestart and
// agent_control.proto's AgentCommand enum comment for why STOP/
// UNINSTALL aren't here yet.
const CommandRestart = "restart"

func validCommand(c string) bool {
	return c == CommandRestart
}

// ConfigOverride is the remotely-editable subset of an agent's config --
// a plain-Go mirror of ingest/internal/agentregistry's overrideFields
// and agent_control.proto's DesiredOverride. Deliberately duplicated
// rather than imported across the module boundary, same convention as
// every other cross-module shared shape in this codebase (see
// grpcserver.TenantIDHeaderKey, enterprise/internal/apiconfig.AIConfig).
// Keep the three in sync by hand.
type ConfigOverride struct {
	BatchMaxSize         *int64  `json:"batch_max_size,omitempty"`
	BatchFlushIntervalMS *int64  `json:"batch_flush_interval_ms,omitempty"`
	HeartbeatEnabled     *bool   `json:"heartbeat_enabled,omitempty"`
	HeartbeatIntervalMS  *int64  `json:"heartbeat_interval_ms,omitempty"`
	JournaldUnit         *string `json:"journald_unit,omitempty"`
}

type Agent struct {
	ID                     string          `json:"id"`
	TenantID               string          `json:"tenant_id"`
	Host                   string          `json:"host"`
	Service                string          `json:"service"`
	AgentVersion           string          `json:"agent_version"`
	SourceKind             string          `json:"source_kind"`
	SourceDetail           string          `json:"source_detail"`
	BatchMaxSize           int64           `json:"batch_max_size"`
	BatchFlushIntervalMS   int64           `json:"batch_flush_interval_ms"`
	HeartbeatEnabled       bool            `json:"heartbeat_enabled"`
	HeartbeatIntervalMS    int64           `json:"heartbeat_interval_ms"`
	FirstSeenAt            time.Time       `json:"first_seen_at"`
	LastSeenAt             time.Time       `json:"last_seen_at"`
	DesiredOverride        *ConfigOverride `json:"desired_override,omitempty"`
	DesiredOverrideVersion string          `json:"desired_override_version,omitempty"`
	AppliedOverrideVersion string          `json:"applied_override_version"`
	// Pending is computed, not stored: an override exists
	// (DesiredOverrideVersion != "") that the agent hasn't reported
	// applying yet (AppliedOverrideVersion doesn't match). This is what
	// the web UI's "pending"/"applied" indicator (task selected:
	// "+ Remote config editing") reads directly, rather than
	// recomputing the same string comparison itself.
	Pending   bool   `json:"pending"`
	UpdatedBy string `json:"updated_by,omitempty"`
	// PendingCommand is "" when nothing is queued, or CommandRestart
	// while a restart hasn't yet been delivered to the agent. Unlike
	// Pending (config), there's no way to observe "delivered" from this
	// table alone -- ingest clears pending_command the instant it hands
	// the command out (see ingest/internal/agentregistry.Registry.
	// CheckIn), so PendingCommand flipping back to "" just as plausibly
	// means "delivered a moment ago" as "never issued." CommandIssuedAt
	// is what the web UI shows instead, as a last-issued record.
	PendingCommand  string     `json:"pending_command,omitempty"`
	CommandIssuedAt *time.Time `json:"command_issued_at,omitempty"`
	CommandIssuedBy string     `json:"command_issued_by,omitempty"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const selectColumns = `
	id, tenant_id, host, service,
	reported_agent_version, reported_source_kind, reported_source_detail,
	reported_batch_max_size, reported_batch_flush_ms,
	reported_heartbeat_on, reported_heartbeat_ms,
	first_seen_at, last_seen_at,
	desired_override, desired_override_version, applied_override_version, updated_by,
	pending_command, command_issued_at, command_issued_by`

func (s *Store) List(ctx context.Context, tenantID string) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+selectColumns+`
		FROM agents WHERE tenant_id = $1 ORDER BY host`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, tenantID, host string) (*Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+selectColumns+`
		FROM agents WHERE tenant_id = $1 AND host = $2`, tenantID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	a, err := scanAgent(rows)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// SetOverride writes a new desired override for host, generating a
// fresh version stamp -- overwrites any previous override wholesale
// (this is "set the desired config," not "patch a few fields into
// whatever was there," so a caller building a partial edit must have
// already merged it against the current value, same as any other PUT
// endpoint in this codebase). Returns ErrNotFound if the agent has
// never checked in (nothing to target an override at yet -- an
// override for a host ingest has never seen would be silently
// unreachable).
func (s *Store) SetOverride(ctx context.Context, tenantID, host string, override ConfigOverride, updatedBy string) (*Agent, error) {
	version := newVersion()
	data, err := json.Marshal(override)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents SET desired_override = $1, desired_override_version = $2, updated_by = $3
		WHERE tenant_id = $4 AND host = $5`,
		data, version, updatedBy, tenantID, host)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, tenantID, host)
}

// IssueCommand queues a one-shot lifecycle command for host, delivered
// on its next CheckIn and cleared atomically by ingest the instant
// that happens (see ingest/internal/agentregistry.Registry.CheckIn) --
// unlike SetOverride, there's no "applied" confirmation to wait for,
// since a restarting agent's process is gone before it could send one.
// command_issued_at/by are overwritten on every call, forming a
// last-issued record even after pending_command itself clears.
func (s *Store) IssueCommand(ctx context.Context, tenantID, host, command, issuedBy string) (*Agent, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents SET pending_command = $1, command_issued_at = now(), command_issued_by = $2
		WHERE tenant_id = $3 AND host = $4`,
		command, issuedBy, tenantID, host)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, tenantID, host)
}

// ClearOverride reverts an agent to running its local agent.toml
// untouched -- the next CheckIn gets has_override=false.
func (s *Store) ClearOverride(ctx context.Context, tenantID, host string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents SET desired_override = NULL, desired_override_version = NULL, updated_by = NULL
		WHERE tenant_id = $1 AND host = $2`, tenantID, host)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgent(row rowScanner) (Agent, error) {
	var a Agent
	var desiredOverride []byte
	var desiredVersion, updatedBy, pendingCommand, commandIssuedBy *string
	if err := row.Scan(
		&a.ID, &a.TenantID, &a.Host, &a.Service,
		&a.AgentVersion, &a.SourceKind, &a.SourceDetail,
		&a.BatchMaxSize, &a.BatchFlushIntervalMS,
		&a.HeartbeatEnabled, &a.HeartbeatIntervalMS,
		&a.FirstSeenAt, &a.LastSeenAt,
		&desiredOverride, &desiredVersion, &a.AppliedOverrideVersion, &updatedBy,
		&pendingCommand, &a.CommandIssuedAt, &commandIssuedBy,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Agent{}, ErrNotFound
		}
		return Agent{}, err
	}
	if updatedBy != nil {
		a.UpdatedBy = *updatedBy
	}
	if pendingCommand != nil {
		a.PendingCommand = *pendingCommand
	}
	if commandIssuedBy != nil {
		a.CommandIssuedBy = *commandIssuedBy
	}
	if desiredVersion != nil {
		a.DesiredOverrideVersion = *desiredVersion
		a.Pending = *desiredVersion != a.AppliedOverrideVersion
		if len(desiredOverride) > 0 {
			var override ConfigOverride
			if err := json.Unmarshal(desiredOverride, &override); err != nil {
				return Agent{}, err
			}
			a.DesiredOverride = &override
		}
	}
	return a, nil
}

// newVersion is an opaque, monotonically-informative-enough stamp for
// DesiredOverride.version -- a timestamp, not a counter, since Store
// has no prior version to increment from without an extra read. Never
// interpreted as a real time value by the agent (see
// agent_control.proto's DesiredOverride.version comment) -- just needs
// to change on every edit.
func newVersion() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
