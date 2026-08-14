-- Correction to a Phase 3 gap found during Phase 4 planning: alert_state
-- never got a tenant_id column, unlike alert_rules/dashboards/
-- notification_targets. Backfilled via a join through alert_rules.id
-- (its owning rule's tenant), not blindly defaulted, even though in
-- practice every pre-Phase-4 row's rule already belongs to 'default'.
-- Bundled as one migration (add nullable -> backfill -> enforce NOT
-- NULL) since it's one logical schema change, same shape as
-- /storage/migrations/0002_add_record_id.sql bundling an ADD COLUMN
-- with its index in one file.
ALTER TABLE alert_state ADD COLUMN IF NOT EXISTS tenant_id TEXT;

UPDATE alert_state
SET tenant_id = alert_rules.tenant_id
FROM alert_rules
WHERE alert_state.rule_id = alert_rules.id
  AND alert_state.tenant_id IS NULL;

ALTER TABLE alert_state ALTER COLUMN tenant_id SET NOT NULL
