# storage

ClickHouse schema and migration tooling for Sentry's analytical store.

## Schema

One table for Phase 0, `logs`:

```sql
CREATE TABLE logs
(
    `timestamp`  DateTime64(9, 'UTC'),
    `host`       String,
    `service`    String,
    `severity`   LowCardinality(String),
    `message`    String,
    `attributes` Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (service, timestamp)
```

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
docker compose up -d          # starts a standalone ClickHouse for local work
./migrate.sh                  # applies migrations/*.sql
```

Environment variables `migrate.sh` reads (all optional, matching
`/ingest`'s ClickHouse defaults so the two stay in sync out of the box):

| Var | Default |
|---|---|
| `CLICKHOUSE_HTTP` | `http://localhost:8123` |
| `CLICKHOUSE_USER` | `default` |
| `CLICKHOUSE_PASSWORD` | (empty) |
| `CLICKHOUSE_DATABASE` | `sentry` |

There's also a `Dockerfile` (bash + curl baked in, `migrations/` copied in
at build time) used by the root-level `docker-compose.yml` as a one-shot
init service — no runtime package install, no host volume mount needed.

## Adding a migration

Add `migrations/000N_description.sql` with the next sequential number and
a single DDL statement. `migrate.sh` picks it up automatically — no
registration step.
