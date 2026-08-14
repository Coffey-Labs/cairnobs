-- Every Phase 0-3 row (dashboards, alert_rules, notification_targets --
-- see their tenant_id DEFAULT 'default' columns) belongs to this tenant.
-- Marked 'active' immediately: this data already exists and is already
-- being served, unlike a genuinely new tenant that must pass through
-- provisioning first.
INSERT INTO tenants (id, display_name, status)
VALUES ('default', 'Default', 'active')
ON CONFLICT (id) DO NOTHING
