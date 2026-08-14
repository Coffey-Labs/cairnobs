CREATE TABLE IF NOT EXISTS tenants
(
    id                       TEXT PRIMARY KEY,
    display_name             TEXT NOT NULL,
    -- Provisioning state machine from /docs/phase-4-isolation-design.md:
    -- every tenant-resolution path must refuse to serve a tenant not in
    -- 'active' state, checked server-side against this column.
    status                   TEXT NOT NULL DEFAULT 'provisioning'
                                 CHECK (status IN ('provisioning', 'active', 'suspended', 'deprovisioning')),
    clickhouse_database_name TEXT,
    tantivy_index_path       TEXT,
    -- Nullable until the first Owner exists -- a tenant can be created
    -- (provisioning) before any user has logged in to claim ownership.
    owner_user_id            UUID REFERENCES users(id),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
)
