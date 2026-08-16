# Phase 5 runbook

Extends `/docs/phase-0-runbook.md` through `/docs/phase-4-runbook.md`.
Read those first. Phase 5 touched only `/web` and the two narrow,
justified backend changes called out below — no new services, no new
pinned-stack components.

## What's actually been verified

Every claim below was checked against a live `docker compose up`
stack with real seeded data (not fixture files, not mocked stores),
in a real browser (Claude in Chrome), not just `npm run check` and
`npm run build` passing. Five real bugs were found this way — two of
them backend bugs unrelated to the frontend redesign itself, only
surfaced because getting real chart/panel/alert data required actually
exercising the write paths those bugs were in. That's the same
"a passing check and a working feature are not the same claim"
discipline every prior phase's runbook has held to.

## 1. Bring up the stack

```sh
docker compose up -d --build
cd web && npm run dev   # localhost:5183, talks to localhost:8080/8081 by default
```

No new services this phase. `docker compose ps` should show the same
set as Phase 4.

## 2. Seed real data

`hack/benchmark-fixture` (Phase 2) still works unchanged:

```sh
cd hack/benchmark-fixture
go run . -count=5000 -batch-size=500 -concurrency=4
```

Create a dashboard covering all 6 panel viz types and an alert rule
with a real notification target, either through the web UI or via the
API directly. One thing worth knowing if you script this the way this
runbook's own verification did: the query language requires a leading
base term before any pipe stage — `stats count by host` alone is a
syntax error (`unexpected COMMA after query` or similar), because
`Parse()` always parses a base filter/free-text term before it'll
accept a `|`. `earliest=-1h | stats count by host` is the idiomatic
"match everything in the current window" form (see
`/docs/query-language-reference.md`'s `earliest=-24h severity=ERROR |
stats count by service` example) — this isn't new to Phase 5, but is
easy to trip over when scripting panel creation instead of using the
query bar, which always has a real base term from its own default
query.

## 3. Dashboard panels — all 6 viz types

Time-series line, bar, single-stat, top-N, table were all real Phase 3
capabilities re-rendered on the new chart layer; heatmap is the one new
type this phase added. Verify each renders against real query results,
not just the `/dev/charts` synthetic fixture route (unlisted, dev-only —
useful for perf testing, not a substitute for exercising the real panel
CRUD + query path).

**Real bug caught here**: `single_stat` panels rendered `0` for every
query, including ones the API confirmed returned real data (`{"columns":
["count"],"rows":[[5000]]}` from `POST /query` showed `5000`, the panel
showed `0`). Root cause in `web/src/lib/charts/pivot.ts`'s value-column
auto-detection: `Array.prototype.findIndex` returns `-1` for "not
found," and the fallback used `??`, which only substitutes on
`null`/`undefined` — `-1 ?? fallback` evaluates to `-1`, not the
fallback. For a single-column result (`stats count`, single-stat's most
common shape), the x-column fallback (index 0) excludes the only column
from the search, `findIndex` always returns `-1`, and the panel silently
read `row[-1]` (`undefined`) as its value. Fixed by checking for `-1`
explicitly instead of relying on `??`. This would not have been caught
by `/dev/charts`'s synthetic multi-column fixture data — it only
reproduces with a genuinely single-column result, which only a real
`stats count`-shaped query against the live API produces.

**Real bug caught here, backend**: creating a heatmap panel returned
`{"error":"viz_type must be one of table, line, bar, single_stat,
top_n, heatmap, got \"heatmap\""}`-shaped failures at two different
layers in sequence. First, the running `api` container was stale
relative to the Phase 5 source change adding `heatmap` to
`validVizType()` — rebuilding (`docker compose build api`) fixed that.
Second, after rebuilding, panel creation still failed with a Postgres
`23514` check-constraint violation: `dashboard_panels`'s `viz_type`
CHECK constraint (`metadata/migrations/0002_create_dashboard_panels.sql`)
was never updated alongside the Go validator, so `heatmap` passed API
validation and then failed on insert. Fixed with a new migration,
`metadata/migrations/0035_add_heatmap_viz_type.sql` (drop and recreate
the constraint — Postgres has no `ALTER CHECK`). Worth remembering for
any future `VizType` addition: it's a three-place change (Go validator,
TS union, DB constraint), not two.

## 4. Alert rule creation

**Real bug caught here, backend, unrelated to the Phase 5 redesign
itself**: creating any alert rule against a freshly migrated database
failed with `inserting initial alert_state: ERROR: null value in
column "tenant_id" of relation "alert_state" violates not-null
constraint`. Phase 4 added `tenant_id` to `alert_state` and
`delivery_log` (migrations `0022`/`0023`, backfilled via a join through
`alert_rules.id`, then set `NOT NULL`) but
`alerting/internal/rulestore/store.go`'s `Create` and `ApplyTransition`
were never updated to populate it on new inserts — every existing row
had a value from the backfill, so this was invisible until the first
rule created *after* that migration ran, which nothing in Phase 4's own
verification happened to do (Phase 4's Docker access was lost partway
through, per its own runbook's disclosed gap). Fixed: `Create`'s
`alert_state` insert now passes `r.TenantID` explicitly;
`ApplyTransition`'s `delivery_log` insert resolves it via `(SELECT
tenant_id FROM alert_rules WHERE id = $1)` since that function only
receives a rule ID, not a full `Rule`. Confirmed fixed against the live
stack: rule creation, evaluation, firing, and a real delivery attempt
(to a fake Slack webhook URL — a real HTTP 404 back, logged in
`delivery_log`, which is itself the intended behavior for an
unreachable target) all completed end to end.

This means **every core-mode alert rule created against a Phase-4- or
Phase-5-migrated database before this fix was silently broken** — worth
knowing if debugging an environment provisioned between those two
points.

## 5. Accessibility sweep

Automated: axe-core, injected into the live app via a temporary
`web/static/axe-test-temp.js` script tag (removed before commit — do
not check it in; recreate from `node_modules/axe-core/axe.min.js` if you
need to re-run this). Checked every route, including the ones that only
render meaningfully with real backend data: `/`, `/dashboards`,
`/dashboards/[id]` (with all 6 panel types populated), `/alerts`,
`/alerts/[id]` (with a real firing rule and delivery history),
`/alerts/new`, `/settings`, `/data-sources`, `/select-tenant`,
`/dev/charts`. Real findings and fixes are cataloged in
`/docs/design-system.md`'s Accessibility section — don't duplicate that
list here; the summary is zero outstanding violations across all of the
above, in both themes, confirmed by re-running axe after each fix
rather than assuming a fix worked.

Manual: keyboard-only tab-order walk through the sidebar, command
palette, query editor, and results table (see the design-system doc's
Accessibility section for the one real gap this caught that axe
structurally can't — a custom interactive element with no keyboard
handler at all doesn't trip any axe rule, since axe checks markup/ARIA
correctness, not "does this thing respond to Enter").

## 6. What's not verified

Same category of gap as every prior phase's disclosed limitations, not
a new one: real external IdP login, a real multi-container Kubernetes
deployment, and load-testing the chart layer against ClickHouse-scale
(millions-of-rows) result sets rather than the 30K-row synthetic
`/dev/charts` stress fixture — that fixture validates client-side
render performance, not query performance at that scale, which is
already covered by Phase 2's own benchmark.
