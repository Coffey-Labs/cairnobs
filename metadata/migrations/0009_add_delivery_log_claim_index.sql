CREATE INDEX IF NOT EXISTS delivery_log_claim_idx ON delivery_log (status, next_attempt_at)
    WHERE status IN ('pending', 'retrying')
