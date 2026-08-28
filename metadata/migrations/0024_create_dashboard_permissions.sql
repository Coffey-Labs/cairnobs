-- Additive-only per-resource grant: lets a specific user exceed their
-- tenant-baseline role on one dashboard (e.g. an Editor granted Admin on
-- a dashboard they don't own). No deny-overrides -- named non-goal in
-- /docs/phase-4-rbac-design.md and PROJECT-SPEC.md's Phase 4 exit criteria.
CREATE TABLE IF NOT EXISTS dashboard_permissions
(
    id           UUID PRIMARY KEY,
    dashboard_id UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin')),
    granted_by   UUID REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dashboard_id, user_id)
)
