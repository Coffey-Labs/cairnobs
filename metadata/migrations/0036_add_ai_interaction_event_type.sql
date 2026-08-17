-- Phase 7 task 12: AI-assisted interactions (translate/fix/optimize
-- accept-or-dismiss decisions) are audited through the *same*
-- append-only, hash-chained audit_log table Phase 4 built -- not a new
-- table -- since audit_log's detail JSONB column and event_type
-- discriminator were already designed to carry event shapes other than
-- "query" (role_change/grant_change/etc. already don't populate
-- row_count/duration_ms, relying on detail instead; ai_interaction is
-- the same shape of extension). Postgres has no ALTER CHECK, so drop
-- and recreate, same pattern as 0035_add_heatmap_viz_type.sql.
ALTER TABLE audit_log DROP CONSTRAINT audit_log_event_type_check;

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_event_type_check
    CHECK (event_type IN ('query', 'role_change', 'grant_change', 'sso_config_change', 'secret_reveal', 'ai_interaction'));
