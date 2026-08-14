CREATE INDEX IF NOT EXISTS audit_log_tenant_created_at_idx ON audit_log (tenant_id, created_at DESC)
