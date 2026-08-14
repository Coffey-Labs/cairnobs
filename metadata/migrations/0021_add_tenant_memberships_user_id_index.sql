-- Supports "which tenants does this user belong to" (session/authorize
-- lookups), the reverse direction from the UNIQUE(tenant_id, user_id)
-- constraint's own index.
CREATE INDEX IF NOT EXISTS idx_tenant_memberships_user_id ON tenant_memberships (user_id)
