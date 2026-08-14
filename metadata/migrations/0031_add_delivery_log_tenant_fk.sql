ALTER TABLE delivery_log ADD CONSTRAINT fk_delivery_log_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
