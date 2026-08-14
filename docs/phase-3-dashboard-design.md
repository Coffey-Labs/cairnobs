# Dashboard design

> **Status:** Approved, in progress. Task 2 of Phase 3 — see `/CLAUDE.md`'s
> "What done looks like for Phase 3" section for the exit criteria this is
> built against. Task 3 (dashboard CRUD API + web UI) implements this
> doc; if implementation reveals this design is wrong somewhere, fix this
> doc in the same change, don't let them drift apart — same discipline as
> `/docs/query-language-design.md`.

## Why this design, in one paragraph

A dashboard is a named collection of panels, each wrapping one saved
Phase 2 query plus a visualization type and a grid position. That's a
straightforward data model; the one real design decision here is *where
it's stored*. Dashboards and their panels need real create/update/delete
semantics with immediate read-your-writes consistency for a UI —
ClickHouse's MergeTree family isn't built for that access pattern (see
"Why not ClickHouse" below). This doc also covers the mechanism that
makes one query language work for both ad hoc search and reusable
dashboard panels: injecting the dashboard's time range into a stored
query at execution time, rather than baking a fixed time range into the
saved query itself.

## Why not ClickHouse: PostgreSQL for control-plane config

This phase adds PostgreSQL as a new pinned-stack component — flagged and
confirmed with the project owner before implementation, per CLAUDE.md's
"ask before... making an architectural decision not already specified in
`/docs/architecture.md`." Scope is strictly control-plane config
(dashboards, panels, and — per `/docs/phase-3-alerting-design.md` —
notification targets, alert rules, alert state, delivery log). Log data
itself is untouched: ClickHouse and Tantivy remain the only stores for
`logs`.

Two concrete reasons ClickHouse doesn't fit this job, not just "it feels
risky":

1. **No row-level locking primitive.** The alerting evaluator (see the
   alerting design doc) needs to atomically claim a due rule so a second
   evaluation tick can't touch it concurrently — a `SELECT ... FOR UPDATE
   SKIP LOCKED` operation. `ReplacingMergeTree` + `FINAL` gives
   eventually-consistent "latest write wins," not concurrency control.
   That's a missing primitive, not a tuning problem.
2. **Read-your-writes consistency for a UI.** A user editing a dashboard
   expects to immediately see their own edit reflected back. ClickHouse's
   MergeTree engines don't guarantee this the way a transactional
   database does without extra machinery (`FINAL`, careful ordering) that
   amounts to reimplementing what Postgres already provides natively.

Scope of the addition: new top-level `/metadata` component (naming
mirrors existing precedent — transport=Redpanda, storage=ClickHouse,
search=Tantivy → metadata=Postgres), `postgres:16-alpine` in
docker-compose, `jackc/pgx/v5` as the Go driver in `api` and the new
`alerting` service. No ORM, no `sqlc` — hand-written SQL per store
package, matching the project's existing avoidance of query-generation
machinery (no cobra, no golang-migrate, nothing code-generated from SQL
anywhere in the repo today).

### Migrations: mirror `/storage/migrate.sh`, not a framework

`storage/README.md`'s objection to `golang-migrate` ("premature machinery"
for what's being built) is general, not specific to ClickHouse's
HTTP-interface constraint — six new tables in one change doesn't meet the
bar that would justify revisiting that call. `/metadata/migrate.sh`
mirrors it: bash + `psql -v ON_ERROR_STOP=1 -f <file>` per migration, a
`schema_migrations` tracking table, one DDL object per file (kept for
repo-wide consistency of what a migration "version" means, even though
Postgres itself supports multi-statement transactions unlike ClickHouse's
HTTP interface), applied by a one-shot init container
(`metadata-migrate`) that other services `depends_on: condition:
service_completed_successfully` — same shape as `clickhouse-migrate`.

```
metadata/
  README.md  Dockerfile  migrate.sh  docker-compose.yml
  migrations/
    0001_create_dashboards.sql
    0002_create_dashboard_panels.sql
    0003_create_notification_targets.sql   (alerting design doc)
    0004_create_alert_rules.sql            (alerting design doc)
    0005_create_alert_state.sql            (alerting design doc)
    0006_create_delivery_log.sql           (alerting design doc)
```

All six tables live in one migrations directory / one Postgres database,
even though `api` (dashboards) and `alerting` (rules/targets/state/log)
are the two services that own different tables within it — mirrors how
ClickHouse already hosts tables conceptually "owned" by different
components (`logs` via ingest's consumer, `schema_migrations` via the
migration runner itself) inside one physical database. No shared Go
store code between `api` and `alerting` — each owns hand-written SQL for
the tables it's responsible for; nothing is shared across service
`internal/` trees in this repo except `/proto`, and that precedent holds
here too.

## Schema

All tables carry `tenant_id TEXT NOT NULL DEFAULT 'default'` (populated,
unenforced — see "Not painting Phase 4 into a corner" below) and
`created_by TEXT NOT NULL DEFAULT 'anonymous'` for the same reason.
`updated_at` is app-managed on every UPDATE, no PL/pgSQL trigger —
consistent with the project's avoidance of database-side procedural code.

```sql
CREATE TABLE dashboards (
    id               UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    default_earliest TEXT NOT NULL DEFAULT '-1h',
    default_latest   TEXT NOT NULL DEFAULT 'now',
    created_by       TEXT NOT NULL DEFAULT 'anonymous',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dashboard_panels (
    id                UUID PRIMARY KEY,
    dashboard_id      UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    title             TEXT NOT NULL DEFAULT '',
    query             TEXT NOT NULL,          -- pipe syntax only, no earliest=/latest=
    query_language    TEXT NOT NULL DEFAULT '', -- mirrors queryRequest.Language: ''|sql|spl
    viz_type          TEXT NOT NULL CHECK (viz_type IN ('table','line','bar','single_stat','top_n')),
    viz_config        JSONB NOT NULL DEFAULT '{}', -- e.g. {"x_column":"timestamp","series_column":"host"}
    position_x        INT NOT NULL,
    position_y        INT NOT NULL,
    width             INT NOT NULL,
    height            INT NOT NULL,
    earliest_override TEXT NULL,             -- NULL = inherit dashboard default
    latest_override   TEXT NULL,
    sort_order        INT NOT NULL DEFAULT 0, -- deterministic export ordering
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`query_language` deliberately excludes `'sql'` from being usable in
practice for panels (enforced at the API layer, not the DB CHECK
constraint — see "Raw-SQL panels out of scope" below): the column exists
because it mirrors the existing `/query` request shape exactly, not
because raw SQL panels are supported yet.

`position_x/y/width/height` map 1:1 to the web UI's grid layout library's
own `x/y/w/h` units — no translation layer between stored position and
rendered position.

IDs are app-generated UUIDs (`google/uuid`, already an `api` dependency),
assigned server-side once, matching how `ingest` assigns `record_id` —
not `gen_random_uuid()` via a Postgres extension, to keep ID generation
in one place (Go) rather than split between the app and the database.

## Time-range mechanics

A dashboard has a default time range (`default_earliest`/
`default_latest`, in the query language's own relative syntax, e.g.
`-1h`/`now`). Each panel can override it. Critically, **panel query
strings never contain `earliest=`/`latest=` themselves** — the query a
user writes for a panel is time-range-agnostic (e.g. `service=api |
where status>=500 | stats count by host`), and the effective time range
is resolved and injected at execution time:

```
effective_earliest = panel.earliest_override ?? dashboard.default_earliest
effective_latest   = panel.latest_override   ?? dashboard.default_latest
executed_query      = "earliest={effective_earliest} latest={effective_latest} " + panel.query
```

This works because `earliest=`/`latest=` are ordinary `base_search` terms
in Phase 2's grammar (`term := ... | "earliest" "=" time_expr | "latest"
"=" time_expr`), implicitly AND'd with whatever else is in the query,
and — like every term in `bool_expr` — order-independent. Prepending them
is syntactically identical to a user having typed them first.

**`"now"` is a UI-only sentinel, never injected literally.** The default
`default_latest` value shown in the picker is the human-readable string
`"now"`, but `time_expr` only accepts a quoted absolute timestamp or a
`"-N unit"` relative offset — there is no `now` token in the grammar.
Injecting `latest=now` produces a real compile error (`expected a quoted
absolute timestamp or a relative offset`), found by actually running a
dashboard panel against the live stack, not a hypothetical. The fix:
when the effective latest value is `"now"` (or empty), the `latest=`
clause is omitted from the injected query entirely — which is exactly
what the query language already does to mean "no upper bound" (see
`planner.go`'s handling of an absent `latest` term). `earliest=` has no
equivalent sentinel; a dashboard's `default_earliest` is always a real
`time_expr` value.

**This injection only works for pipe-syntax queries.** A raw-SQL query
(Phase 2's `SELECT ...` escape hatch) has no equivalent injection point —
there's no reliable way to splice a time bound into arbitrary SQL without
parsing it, which is exactly the work the raw-SQL escape hatch was
designed to avoid (see `/docs/query-language-design.md`'s "not parsed,
wrapped as opaque IR"). **Raw-SQL dashboard panels are out of scope for
Phase 3** — a disclosed non-goal, not a silent gap. `dashboard_panels.
query_language` exists in the schema for symmetry with the `/query`
request shape, but the dashboard API layer (task 3) rejects `'sql'` on
create/update with a clear error rather than accepting it and having the
time-range picker silently do nothing.

## Panel execution: client-side, not server-side

`GET /dashboards/{id}` returns panel *definitions* only (query, viz_type,
position, overrides) — it does not execute any queries. The web UI
resolves each panel's effective time range and calls `POST /query`
per panel directly, exactly the same endpoint the root query page already
uses. This keeps `api/internal/dashboards` pure CRUD with zero
query-execution code of its own, consistent with the project's standing
rule that nothing duplicates `querylang`'s execution path (the same
reason `sentryctl query` calls `/query` over HTTP instead of importing
`querylang` internals). It also means panels load and error
independently in the UI — one broken panel query doesn't fail the whole
dashboard.

## Visualization types

- **`table`**: renders `{columns, rows}` directly via (an extended)
  `ResultsTable.svelte`. No `viz_config` needed.
- **`line`** / **`bar`**: `viz_config` names which result column is the
  x-axis/category (`x_column`, typically `timestamp` or a `stats ... by`
  grouping column) and which is plotted (`series_column` for
  multi-series, `value_column` for the plotted value). Rendered via
  uPlot (see below).
- **`single_stat`**: takes the first row's first numeric column, no
  `viz_config` needed for MVP (a labeled big number).
- **`top_n`**: a table sorted/limited view — for MVP this is really
  "table, but the query already did `sort`/`head`," so it renders
  through the same table path as `table` with different framing in the
  UI; no separate execution logic.

### New frontend dependencies

`web/package.json` currently has zero runtime dependencies. Two are
added this phase, both confirmed with the project owner before landing:

- **gridstack.js** — panel grid layout with drag-and-drop/resize.
  Vanilla JS, framework-agnostic (wrapped in a thin Svelte component),
  well-established, used by real dashboard products. Pre-approved
  ("a lightweight grid library is fine, don't build drag-and-drop from
  scratch") — listed here so the concrete choice is visible in the
  design record, not just in a `package.json` diff.
- **uPlot** — line/bar chart rendering. ~45KB, canvas-based, fast, no
  heavy transitive dependency tree. Chosen over Chart.js (heavier) and a
  hand-rolled D3/SVG approach (correct axes/scales/tooltips is a lot of
  code to own well for something a library already does correctly).

Table, single-stat, and top-N panels need neither dependency.

## Export / import

`GET /dashboards/{id}/export` returns the dashboard and its panels
marshaled as one JSON document (the same `Dashboard`/`Panel` Go structs
used internally, not a separate export-specific shape). `POST
/dashboards/import` accepts that same shape and creates a new dashboard
from it (new IDs assigned, not a literal replay of the source IDs, so
importing into a different environment doesn't collide). This is
deliberately the *same* JSON contract `sentryctl dashboards apply` (task
7) consumes — "exportable/importable, Terraform-friendly" isn't a
separate design, it's one JSON shape used from two call sites (web
export button, CLI apply).

## Not painting Phase 4 into a corner

- **`tenant_id`** on every table, populated but unenforced — Phase 4's
  multi-tenancy retrofit needs a partitioning/enforcement layer, not a
  schema migration + backfill on every table.
- **`created_by`** on every table, defaulted to `'anonymous'` since no
  auth exists yet — same reasoning, cheaper to add the column now than
  during Phase 4 when real identity is also landing.
- **"Shareable" currently means "shareable within the single trusted
  deployment."** There is no access-control model at all yet — not
  tenant isolation, not even within-tenant per-dashboard permissions.
  Stating this plainly here so it doesn't read as more solved than it is
  once multi-tenancy is on the table.
- CORS stays wide open (`Access-Control-Allow-Origin: *`) on `api`,
  matching the existing Phase 0–2 posture. Adding dashboards doesn't
  change this tradeoff, just widens the same already-accepted surface
  area — noted so it's not mistaken for a new gap introduced this phase.

## Where this lives

`api/internal/dashboards/` (`handler.go`, `store.go`, `types.go`, plus
tests) — mirrors `api/internal/queryapi`'s existing `Handler`/`Store`
shape. Requires one small prerequisite refactor to existing code:
`queryapi.Handler.Routes()` currently builds its own `http.NewServeMux()`
and applies CORS in one shot; with a second handler package, both need to
register onto one shared mux in `main.go`, with CORS applied once at the
top. `queryapi.Handler` gains a `RegisterRoutes(mux *http.ServeMux)`
method in place of `Routes()`.
