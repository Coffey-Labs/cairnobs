CREATE INDEX IF NOT EXISTS delivery_log_rule_id_created_at_idx ON delivery_log (rule_id, created_at DESC)
