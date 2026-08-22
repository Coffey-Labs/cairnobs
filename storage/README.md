# storage

ClickHouse schema and migration tooling for Cairn OBS's analytical store.

## Schema

One table, `logs` (Phase 0 columns plus `record_id`, added in
`migrations/0002_add_record_id.sql`):

```sql
CREATE TABLE logs
(
    `timestamp`  DateTime64(9, 'UTC'),
    `host`       String,
    `service`    String,
    `severity`   LowCardinality(String),
    `message`    String,
    `attributes` Map(String, String),
    `record_id`  UUID DEFAULT generateUUIDv4()
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (service, timestamp)
-- plus: INDEX record_id_idx record_id TYPE bloom_filter GRANULARITY 4
```

**`record_id`** (Phase 1) is the stable per-record identifier Tantivy's
full-text search joins back to this table with — `/ingest`'s gRPC front
end assigns it once, server-side, before a record is produced to
Redpanda (see `/ingest/README.md` for why it has to happen exactly once,
upstream of both the ClickHouse-writer and Tantivy-indexer consumers).
Added via `ALTER TABLE ... ADD COLUMN` + `ADD INDEX` rather than changing
`ORDER BY`: `ORDER BY (service, timestamp)` is the proven time-range-scan
access pattern from Phase 0 and shouldn't be disturbed for a
fundamentally different access pattern (point lookups by ID). A
data-skipping bloom filter index on `record_id` serves the `WHERE
record_id IN (...)` lookup Tantivy-backed search results need, without
touching the primary sort order. The `DEFAULT generateUUIDv4()` is a
safety net, not the normal path — every row `/ingest` writes explicitly
supplies its own `record_id` from the proto message; the default only
matters for rows written some other way.

Notes on choices that weren't fully specified by the task description:

- **`DateTime64(9, 'UTC')`** (nanosecond precision) rather than second or
  millisecond precision, to match the agent's `timestamp_unix_nano` field
  end to end without truncation.
- **`severity` as `LowCardinality(String)`**, not a numeric OTel
  `SeverityNumber`. `/ingest`'s `normalize` package writes short text
  values (`TRACE`/`DEBUG`/`INFO`/`WARN`/`ERROR`/`FATAL`/`UNSPECIFIED`).
  `LowCardinality` gets you most of the storage/query efficiency of an enum
  without committing to one at the schema level. Splitting into a proper
  `SeverityNumber` + `SeverityText` pair (full OTel shape) is one of the
  open questions already flagged in `/docs/architecture.md`.
- **`PARTITION BY toDate(timestamp)`** (daily partitions) and
  **`ORDER BY (service, timestamp)`** are exactly what the task asked for
  — service-scoped queries over a time range are the dominant access
  pattern this is optimized for.
- No TTL/retention clause yet — also an open question in architecture.md,
  deferred until storage sizing is a real concern.

## Migration tooling: a plain SQL-file runner, not golang-migrate

`migrate.sh` applies `migrations/*.sql` in filename order over
ClickHouse's HTTP interface, tracking what's applied in a
`schema_migrations` table. Chosen over `golang-migrate` for Phase 0
because there's exactly one migration to run — pulling in a migration
framework (another dependency, another thing to configure/vendor) for a
single `CREATE TABLE` is exactly the kind of premature machinery this
project's conventions say to avoid. Revisit `golang-migrate` once there's
real schema churn across environments (rollback support, checksums,
concurrent-apply safety become worth their cost at that point, not before).

**Convention:** one DDL statement per migration file. The ClickHouse HTTP
interface isn't reliably multi-statement, so `migrate.sh` doesn't try to
split multi-statement files — keep each migration to a single statement.

## Running

```sh
docker compose up -d                          # starts a standalone ClickHouse for local work
CLICKHOUSE_PASSWORD=cairnobs-dev-only ./migrate.sh   # applies migrations/*.sql
```

`CLICKHOUSE_PASSWORD` here has to match whatever `docker-compose.yml` set
on the `clickhouse` service — found this the hard way running the Phase 0
runbook for real: the official ClickHouse image silently disables *all*
network access (including the published port, not just container-to-
container traffic) for the `default` user unless `CLICKHOUSE_USER` or
`CLICKHOUSE_PASSWORD` is a genuinely non-empty value. An empty
`CLICKHOUSE_PASSWORD=""` still triggers the lockdown — it has to actually
have a value. Not a real secret, just what this image demands.

Environment variables `migrate.sh` reads (all optional except
`CLICKHOUSE_PASSWORD` as of the above, matching `/ingest`'s ClickHouse
defaults so the two stay in sync out of the box):

| Var | Default |
|---|---|
| `CLICKHOUSE_HTTP` | `http://localhost:8123` |
| `CLICKHOUSE_USER` | `default` |
| `CLICKHOUSE_PASSWORD` | (empty — override, see above) |
| `CLICKHOUSE_DATABASE` | `cairnobs` |

There's also a `Dockerfile` (bash + curl baked in, `migrations/` copied in
at build time) used by the root-level `docker-compose.yml` as a one-shot
init service — no runtime package install, no host volume mount needed.

## Adding a migration

Add `migrations/000N_description.sql` with the next sequential number and
a single DDL statement. `migrate.sh` picks it up automatically — no
registration step.
