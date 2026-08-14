-- Extension point named in /docs/phase-4-rbac-design.md: one row
-- per-tenant today (each tenant has exactly one ClickHouse database +
-- one Tantivy index), not pretending multiple sources per tenant exist
-- yet -- that's real future work, this table just leaves room for it.
CREATE TABLE IF NOT EXISTS data_sources
(
    id                       UUID PRIMARY KEY,
    tenant_id                TEXT NOT NULL REFERENCES tenants(id),
    name                     TEXT NOT NULL,
    clickhouse_database_name TEXT NOT NULL,
    tantivy_index_path       TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
)
