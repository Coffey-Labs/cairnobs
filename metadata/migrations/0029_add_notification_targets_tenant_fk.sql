ALTER TABLE notification_targets ADD CONSTRAINT fk_notification_targets_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
