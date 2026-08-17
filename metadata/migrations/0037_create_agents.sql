-- Agent inventory + remote config (see /docs/agent-management-design.md).
-- One row per (tenant, host), upserted by ingest on every CheckIn RPC.
-- tenant_id defaults to 'default' for single-tenant deployments with no
-- TenantResolver configured, same pattern dashboards/alert_rules already
-- established in Phase 3/4.
--
-- desired_override/desired_override_version are written by api (the web
-- UI's edit form); reported_* and last_seen_at are written by ingest (an
-- agent's own CheckIn). applied_override_version is written by ingest
-- too, but its value comes from the agent itself (CheckInRequest.
-- applied_override_version) -- it's what lets the web UI tell "pending"
-- (desired_override_version != applied_override_version) apart from
-- "applied" without either service needing to poll the other.
CREATE TABLE IF NOT EXISTS agents
(
    id                        UUID PRIMARY KEY,
    tenant_id                 TEXT NOT NULL REFERENCES tenants(id),
    host                      TEXT NOT NULL,
    service                   TEXT NOT NULL DEFAULT '',
    reported_agent_version    TEXT NOT NULL DEFAULT '',
    reported_source_kind      TEXT NOT NULL DEFAULT '',
    reported_source_detail    TEXT NOT NULL DEFAULT '',
    reported_batch_max_size   BIGINT NOT NULL DEFAULT 0,
    reported_batch_flush_ms   BIGINT NOT NULL DEFAULT 0,
    reported_heartbeat_on     BOOLEAN NOT NULL DEFAULT true,
    reported_heartbeat_ms     BIGINT NOT NULL DEFAULT 0,
    first_seen_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL desired_override_version means no override has ever been set
    -- -- CheckInResponse.has_override is false and the agent runs its
    -- local agent.toml untouched.
    desired_override          JSONB,
    desired_override_version  TEXT,
    applied_override_version  TEXT NOT NULL DEFAULT '',
    updated_by                TEXT,
    UNIQUE (tenant_id, host)
)
