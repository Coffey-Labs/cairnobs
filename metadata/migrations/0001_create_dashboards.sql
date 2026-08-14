CREATE TABLE IF NOT EXISTS dashboards
(
    id               UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    default_earliest TEXT NOT NULL DEFAULT '-1h',
    default_latest   TEXT NOT NULL DEFAULT 'now',
    created_by       TEXT NOT NULL DEFAULT 'anonymous',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
)
