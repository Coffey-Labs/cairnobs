CREATE TABLE IF NOT EXISTS audit_log
(
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    user_id       UUID,
    source        TEXT NOT NULL CHECK (source IN ('api', 'web', 'cli', 'alerting')),
    event_type    TEXT NOT NULL CHECK (event_type IN ('query', 'role_change', 'grant_change', 'sso_config_change', 'secret_reveal')),
    query_text    TEXT,
    row_count     INT,
    duration_ms   INT,
    status        TEXT NOT NULL CHECK (status IN ('success', 'error')),
    error_message TEXT,
    detail        JSONB NOT NULL DEFAULT '{}',
    prev_hash     TEXT,
    row_hash      TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
)
