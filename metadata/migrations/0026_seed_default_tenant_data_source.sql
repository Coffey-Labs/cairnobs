-- The 'default' tenant's single data source, pointing at the one
-- ClickHouse database ("sentry") and Tantivy index every Phase 0-3
-- deployment already uses -- see api/internal/config's CLICKHOUSE_DATABASE
-- default and search's index path default.
INSERT INTO data_sources (id, tenant_id, name, clickhouse_database_name, tantivy_index_path)
SELECT '00000000-0000-0000-0000-000000000001', 'default', 'default', 'sentry', '/var/lib/sentry-search'
WHERE NOT EXISTS (SELECT 1 FROM data_sources WHERE tenant_id = 'default')
