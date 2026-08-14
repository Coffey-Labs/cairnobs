CREATE TABLE IF NOT EXISTS notification_targets
(
    id               UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('webhook', 'slack', 'pagerduty')),
    webhook_url      TEXT NOT NULL,
    payload_template TEXT,
    headers          JSONB NOT NULL DEFAULT '{}',
    secret           TEXT,
    created_by       TEXT NOT NULL DEFAULT 'anonymous',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
)
