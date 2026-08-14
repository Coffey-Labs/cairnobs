ALTER TABLE alert_state ADD CONSTRAINT fk_alert_state_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
