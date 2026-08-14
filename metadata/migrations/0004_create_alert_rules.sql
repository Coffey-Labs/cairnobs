CREATE TABLE IF NOT EXISTS alert_rules
(
    id                        UUID PRIMARY KEY,
    tenant_id                 TEXT NOT NULL DEFAULT 'default',
    name                      TEXT NOT NULL,
    description               TEXT NOT NULL DEFAULT '',
    query                     TEXT NOT NULL,
    query_language            TEXT NOT NULL DEFAULT '',
    condition_type            TEXT NOT NULL CHECK (condition_type IN ('threshold', 'absence')),
    comparator                TEXT CHECK (comparator IN ('gt', 'gte', 'lt', 'lte', 'eq', 'ne')),
    threshold_value           DOUBLE PRECISION,
    eval_interval_seconds     INT NOT NULL CHECK (eval_interval_seconds >= 30),
    for_minutes               INT NOT NULL DEFAULT 0,
    renotify_interval_minutes INT,
    notification_target_id    UUID NOT NULL REFERENCES notification_targets(id),
    enabled                   BOOLEAN NOT NULL DEFAULT true,
    created_by                TEXT NOT NULL DEFAULT 'anonymous',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
)
