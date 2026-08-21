package logretention

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentRetentionStore reads the protective retention floors set on
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

// RetentionDaysByHost reports the configured log_retention_days for
// every agent that has one set, keyed by host -- a host absent from
// this map has no configured floor at all. Per-host rather than a
// single global maximum: now that deletion is host-scoped
// (Handler.partitionHosts), a floor on one host must never block
// deleting another host's logs that happen to be requested in the same
// call.
func (s *AgentRetentionStore) RetentionDaysByHost(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host, (desired_override->>'log_retention_days')::int
		FROM agents
		WHERE desired_override->>'log_retention_days' IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var host string
		var days int
		if err := rows.Scan(&host, &days); err != nil {
			return nil, err
		}
		out[host] = days
	}
	return out, rows.Err()
}
