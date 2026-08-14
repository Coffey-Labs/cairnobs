-- 0024_create_dashboard_permissions.sql's CHECK constraint diverged
-- from /docs/phase-4-rbac-design.md's schema: it allowed role='admin'
-- and left granted_by nullable. Neither divergence is meaningful under
-- the enforcement built in Phase 4 task 5 (enterprise/internal/
-- rbacstore's dashboard-permission adapter): a resource-level grant only
-- ever raises someone to Editor on one dashboard (Admin/Owner already
-- have tenant-wide access, so an "admin" grant would be a no-op at best
-- and a confusing dead code path at worst), and every real grant is
-- created by an authenticated Editor/Admin/Owner, so granted_by should
-- never legitimately be null. Found and fixed before any of this ran
-- against a live database in this environment.
ALTER TABLE dashboard_permissions DROP CONSTRAINT dashboard_permissions_role_check;
ALTER TABLE dashboard_permissions ADD CONSTRAINT dashboard_permissions_role_check CHECK (role IN ('viewer', 'editor'));
ALTER TABLE dashboard_permissions ALTER COLUMN granted_by SET NOT NULL;
