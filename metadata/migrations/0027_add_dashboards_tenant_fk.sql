ALTER TABLE dashboards ADD CONSTRAINT fk_dashboards_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
