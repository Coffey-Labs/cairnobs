# metadata

PostgreSQL schema and migration tooling for Sentry's control-plane
config: dashboards, alert rules, and everything else that isn't log data.
See `/docs/phase-3-dashboard-design.md` and
`/docs/phase-3-alerting-design.md` for why this is a separate database
from `/storage` (ClickHouse) rather than new ClickHouse tables — short
version: dashboards and alert state need real row-level locking and
transactional read-modify-write, which ClickHouse's MergeTree family
doesn't provide.

## Schema

Seven tables across three features, one shared database (`sentry_metadata`):

- `dashboards`, `dashboard_panels` — owned by `/api` (`api/internal/dashboards`)
- `notification_targets`, `alert_rules`, `alert_state`, `delivery_log` —
  owned by `/alerting`
- `audit_log` — owned by `enterprise/internal/audit` (Phase 4). Unlike
  every other table here, this one is **not** written through the shared
  `sentry` role/pool — see "The `audit_writer` role" below.

"Owned" here is a documentation convention, not a technical boundary —
both services connect to the same Postgres instance/database, each with
its own hand-written SQL for the tables it's responsible for. Nothing is
shared across service `internal/` trees for this, matching the existing
repo convention that only `/proto` is shared code (and even that isn't
shared logic, just generated bindings).

## The `audit_writer` role: a second, more restricted credential

`audit_log` is append-only by design (see
`/docs/phase-4-isolation-design.md`'s audit-logging section) — a
compliance requirement, not just a convention, so it's backed by two
independent defenses, both verified against a live Postgres, not just
written:

1. A dedicated `audit_writer` Postgres role (`migrations/0012`-`0014`)
   with **only** `INSERT`/`SELECT` grants on `audit_log` — no
   `UPDATE`/`DELETE`/`TRUNCATE`, ever. `enterprise/internal/audit.Store`
   connects using this role's credentials via its **own** `pgxpool.Pool`,
   never the shared `sentry` pool `api`/`alerting`'s other stores use —
   reusing the shared pool for audit writes would give audit_log's
   application-level credential the same `UPDATE`/`DELETE` grants every
   other metadata table has, silently defeating the whole point.
2. A `BEFORE UPDATE OR DELETE` trigger (`migrations/0015`-`0016`) that
   rejects the operation for **any** role, including the table owner
   (`sentry`) — confirmed live: even `sentry` needs to explicitly
   `ALTER TABLE audit_log DISABLE TRIGGER audit_log_immutable` (a
   privileged, distinct-from-normal-access operation) before it can
   modify a row. This is redundant defense-in-depth independent of the
   grant, protecting against a future migration accidentally re-granting
   `UPDATE` to `audit_writer`.

`AUDIT_WRITER_PASSWORD` (default `audit-writer-dev-only`, matching every
other dev-only credential in this repo) sets the role's password at
creation time via `psql -v audit_writer_password=...` substitution in
`migrate.sh` — **not** hardcoded in the migration SQL file itself. One
real gotcha found while building this: psql's `:'var'` substitution does
**not** apply inside a `DO $$ ... $$` dollar-quoted block (by design, so
client-side substitution can't corrupt a function/procedure body) — the
role-creation migration is a plain `CREATE ROLE`, not wrapped in an
`IF NOT EXISTS` check, relying on `schema_migrations` tracking for
idempotency instead (the same pattern Phase 1's non-idempotent
`ALTER TABLE ... ADD COLUMN` migration in `/storage` already used).

## Migration tooling: mirrors `/storage/migrate.sh`, not a framework

Same reasoning as `/storage/README.md`: pulling in `golang-migrate` for
what's currently six `CREATE TABLE` statements is premature machinery.
`migrate.sh` applies `migrations/*.sql` in filename order over `psql`,
tracking what's applied in a `schema_migrations` table, one DDL object
per file (kept for repo-wide consistency of what a migration "version"
means, even though Postgres itself supports multi-statement transactions
unlike ClickHouse's HTTP interface).

## Running

```sh
docker compose up -d                                   # starts a standalone Postgres for local work
POSTGRES_PASSWORD=sentry-dev-only ./migrate.sh          # applies migrations/*.sql
```

Environment variables `migrate.sh` reads (all optional except
`POSTGRES_PASSWORD`, matching the root docker-compose.yml's
`metadata-postgres` service):

| Var | Default |
|---|---|
| `POSTGRES_HOST` | `localhost` |
| `POSTGRES_PORT` | `5432` |
| `POSTGRES_USER` | `sentry` |
| `POSTGRES_PASSWORD` | (empty — must be set) |
| `POSTGRES_DATABASE` | `sentry_metadata` |
| `AUDIT_WRITER_PASSWORD` | `audit-writer-dev-only` |

The database itself isn't created by `migrate.sh` — the `postgres:16-alpine`
image auto-creates `POSTGRES_DB` on first startup, unlike ClickHouse where
`migrate.sh` has to issue `CREATE DATABASE IF NOT EXISTS` itself.

There's also a `Dockerfile` (bash + the `postgresql16-client` package
baked in, `migrations/` copied in at build time) used by the root-level
`docker-compose.yml` as a one-shot init service (`metadata-migrate`) —
no runtime package install, no host volume mount needed.

## Adding a migration

Add `migrations/000N_description.sql` with the next sequential number and
a single DDL statement. `migrate.sh` picks it up automatically — no
registration step.
