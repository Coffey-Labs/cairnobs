package logretention

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentRetentionStore reads the protective retention floor set on
// agents.ConfigOverride.LogRetentionDays -- a separate, Postgres-backed
// concern from Store's ClickHouse access above, so it lives in its own
// file. Deliberately its own narrow query against the same `agents`
// table api/agents.Store manages, rather than importing api/agents for
// a shared type, matching this codebase's "each package owns direct
// SQL access to what it needs" convention (e.g. alerting and api both
// read dashboards-adjacent tables independently rather than sharing a
// store type across a package boundary).
type AgentRetentionStore struct {
	pool *pgxpool.Pool
}

func NewAgentRetentionStore(pool *pgxpool.Pool) *AgentRetentionStore {
	return &AgentRetentionStore{pool: pool}
}

// MaxRetentionDays reports the largest log_retention_days configured
// across any agent's desired_override, if any are set at all -- this is
// the floor a non-owner's deletion request must not reach into (see
// Handler.checkRetentionFloor). The second return value is false when
// no agent has this field configured, distinct from a configured floor
// of 0 (which validateOverride never allows to be stored in the first
// place).
func (s *AgentRetentionStore) MaxRetentionDays(ctx context.Context) (int, bool, error) {
	var days *int
	err := s.pool.QueryRow(ctx, `
		SELECT max((desired_override->>'log_retention_days')::int)
		FROM agents
		WHERE desired_override->>'log_retention_days' IS NOT NULL`).Scan(&days)
	if err != nil {
		return 0, false, err
	}
	if days == nil {
		return 0, false, nil
	}
	return *days, true, nil
}
