-- Same gap and same fix as 0022, for delivery_log.
ALTER TABLE delivery_log ADD COLUMN IF NOT EXISTS tenant_id TEXT;

UPDATE delivery_log
SET tenant_id = alert_rules.tenant_id
FROM alert_rules
WHERE delivery_log.rule_id = alert_rules.id
  AND delivery_log.tenant_id IS NULL;

ALTER TABLE delivery_log ALTER COLUMN tenant_id SET NOT NULL
