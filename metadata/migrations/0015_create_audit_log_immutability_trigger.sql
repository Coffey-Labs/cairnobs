CREATE OR REPLACE FUNCTION audit_log_deny_update_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP;
END
$$ LANGUAGE plpgsql
