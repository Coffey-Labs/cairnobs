ALTER TABLE alert_rules ADD CONSTRAINT fk_alert_rules_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
