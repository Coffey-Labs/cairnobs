CREATE TABLE IF NOT EXISTS alert_state
(
    rule_id              UUID PRIMARY KEY REFERENCES alert_rules(id) ON DELETE CASCADE,
    state                TEXT NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'pending', 'firing')),
    condition_true_since TIMESTAMPTZ,
    fired_at             TIMESTAMPTZ,
    last_notified_at     TIMESTAMPTZ,
    last_evaluated_at    TIMESTAMPTZ,
    last_eval_status     TEXT NOT NULL DEFAULT 'ok' CHECK (last_eval_status IN ('ok', 'error')),
    last_error           TEXT,
    last_value           DOUBLE PRECISION,
    consecutive_errors   INT NOT NULL DEFAULT 0,
    next_eval_at         TIMESTAMPTZ NOT NULL,
    claimed_at           TIMESTAMPTZ
)
