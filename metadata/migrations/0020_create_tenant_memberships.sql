-- A user's role is per-tenant, not global -- see
-- /docs/phase-4-rbac-design.md's role matrix (Viewer/Editor/Admin/Owner).
-- One row per (tenant, user); a user with no row for a tenant has no
-- access to it at all (default-deny, not default-viewer).
CREATE TABLE IF NOT EXISTS tenant_memberships
(
    id         UUID PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin', 'owner')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, user_id)
)
