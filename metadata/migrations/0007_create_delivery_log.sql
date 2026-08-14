CREATE TABLE IF NOT EXISTS delivery_log
(
    id                     BIGSERIAL PRIMARY KEY,
    rule_id                UUID NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    notification_target_id UUID NOT NULL REFERENCES notification_targets(id),
    event_type             TEXT NOT NULL CHECK (event_type IN ('firing', 'resolved')),
    status                 TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'retrying')),
    attempt_count          INT NOT NULL DEFAULT 0,
    max_attempts           INT NOT NULL DEFAULT 5,
    next_attempt_at        TIMESTAMPTZ,
    last_attempt_at        TIMESTAMPTZ,
    last_error             TEXT,
    response_status        INT,
    payload                JSONB NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
)
