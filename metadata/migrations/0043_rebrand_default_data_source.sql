-- Repoint the 'default' tenant's data source at the post-rebrand
-- ClickHouse database and Tantivy index path.
--
-- 0026 seeded this row with ('sentry', '/var/lib/sentry-search'),
-- correct at the time: it deliberately mirrored what api/internal/config
-- then defaulted CLICKHOUSE_DATABASE to, and search's index path
-- default. The Sentry -> Cairn OBS rebrand later moved both defaults --
-- api/internal/config and ingest/internal/config now default to
-- "cairnobs", and every index path in the tree is
-- /var/lib/cairnobs-search (docker-compose's search-index-data mount,
-- the Helm chart's search.yaml, and the per-tenant paths
-- enterprise/cmd/enterprise-api builds) -- but the already-applied
-- 0026 row did not move with them.
--
-- The result on any database where 0026 ran: the default tenant's data
-- source names a ClickHouse database nothing writes to, and an index
-- path nothing maintains. Editing 0026 in place would not fix those
-- deployments, since it has already been recorded as applied -- hence a
-- forward migration.
--
-- Scoped by the exact stale values rather than by tenant_id alone, so
-- this is a no-op on:
--   - fresh databases, where 0026's seed already reflects current
--     defaults if it is ever re-run,
--   - deployments that set CLICKHOUSE_DATABASE explicitly and corrected
--     this row by hand.
-- It will not clobber a deliberately-chosen database name.
UPDATE data_sources
SET clickhouse_database_name = 'cairnobs'
WHERE tenant_id = 'default'
  AND name = 'default'
  AND clickhouse_database_name = 'sentry';

UPDATE data_sources
SET tantivy_index_path = '/var/lib/cairnobs-search'
WHERE tenant_id = 'default'
  AND name = 'default'
  AND tantivy_index_path = '/var/lib/sentry-search';
