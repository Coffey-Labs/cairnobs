-- Agent lifecycle commands are audited through the same append-only,
-- hash-chained audit_log table every other privileged action already
-- uses -- same extension shape as 0036_add_ai_interaction_event_type.sql.
-- Postgres has no ALTER CHECK, so drop and recreate.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_event_type_check;

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_event_type_check
    CHECK (event_type IN ('query', 'role_change', 'grant_change', 'sso_config_change', 'secret_reveal', 'ai_interaction', 'agent_command'));
