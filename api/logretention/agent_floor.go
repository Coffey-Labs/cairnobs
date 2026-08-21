package logretention

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HostFloor is one host's configured retention protection --
// DefaultDays (from api/agents.ConfigOverride.LogRetentionDays) applies
// to any service on this host with no more specific entry;
// ServiceDays (from ConfigOverride.ServiceLogRetentionDays) overrides
// that default for the services named in it. Either may be absent
// independently (a host can have a service override with no host
// default, or vice versa).
type HostFloor struct {
	DefaultDays *int
	ServiceDays map[string]int
}

// AgentRetentionStore reads the protective retention floors set on
// agents.ConfigOverride -- a separate, Postgres-backed concern from
// Store's ClickHouse access above, so it lives in its own file.
// Deliberately its own narrow query against the same `agents` table
// api/agents.Store manages, rather than importing api/agents for a
// shared type, matching this codebase's "each package owns direct SQL
// access to what it needs" convention (e.g. alerting and api both read
// dashboards-adjacent tables independently rather than sharing a store
// type across a package boundary).
type AgentRetentionStore struct {
	pool *pgxpool.Pool
}

func NewAgentRetentionStore(pool *pgxpool.Pool) *AgentRetentionStore {
	return &AgentRetentionStore{pool: pool}
}

// FloorsByHost reports every host with a configured floor of either
// kind (host-level default, per-service, or both), keyed by host -- a
// host absent from this map has no configured floor at all.
func (s *AgentRetentionStore) FloorsByHost(ctx context.Context) (map[string]HostFloor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host,
			desired_override->>'log_retention_days',
			(desired_override->'service_log_retention_days')::text
		FROM agents
		WHERE desired_override->>'log_retention_days' IS NOT NULL
		   OR desired_override->'service_log_retention_days' IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]HostFloor{}
	for rows.Next() {
		var host string
		var defaultDaysText, serviceDaysJSON *string
		if err := rows.Scan(&host, &defaultDaysText, &serviceDaysJSON); err != nil {
			return nil, err
		}
		var hf HostFloor
		if defaultDaysText != nil {
			d, err := strconv.Atoi(*defaultDaysText)
			if err != nil {
				return nil, fmt.Errorf("parsing log_retention_days for host %q: %w", host, err)
			}
			hf.DefaultDays = &d
		}
		if serviceDaysJSON != nil {
			hf.ServiceDays = map[string]int{}
			if err := json.Unmarshal([]byte(*serviceDaysJSON), &hf.ServiceDays); err != nil {
				return nil, fmt.Errorf("parsing service_log_retention_days for host %q: %w", host, err)
			}
		}
		out[host] = hf
	}
	return out, rows.Err()
}

// Effective reports the retention floor that applies to service on
// this host: a service-specific override if one exists, otherwise the
// host-level default, otherwise no floor at all.
func (hf HostFloor) Effective(service string) (int, bool) {
	if days, ok := hf.ServiceDays[service]; ok {
		return days, true
	}
	if hf.DefaultDays != nil {
		return *hf.DefaultDays, true
	}
	return 0, false
}
