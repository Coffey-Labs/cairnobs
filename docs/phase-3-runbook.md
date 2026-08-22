# Phase 3 runbook

Extends `/docs/phase-0-runbook.md` through `/docs/phase-2-runbook.md`
with dashboards and alerting. Read those first — this assumes the stack
already works. Phase 3 adds a new Postgres-backed control-plane
(`/metadata`), a new dashboard CRUD surface in `/api`, and a new
top-level service (`/alerting`) plus its web UI and CLI subcommands.

## What's actually been verified

Every claim below was checked against the live stack, not asserted —
same discipline as every prior phase's runbook. This phase's real
findings (five of them, not hypothetical) are called out inline where
they were caught, matching the project's standing "actually run it"
rule: a passing test suite and a working feature are not the same claim.

## 1. Bring up the stack

```sh
docker compose up -d --build
```

New services beyond Phase 0–2: `metadata-postgres`, `metadata-migrate`
(one-shot, applies `/metadata/migrations/*.sql`), `alerting` (port 8081).
`api` now depends on `metadata-migrate` and exposes a `-healthcheck`
self-check mode (distroless, no shell/wget — see `api/cmd/api/main.go`)
so `alerting`'s `depends_on: api: condition: service_healthy` means
something real. Confirm everything is healthy:

```sh
docker compose ps
# api, alerting, metadata-postgres, clickhouse, redpanda should all show (healthy)
```

## 2. Dashboards

Generate a fixture dataset (reuses Phase 2's `hack/benchmark-fixture`):

```sh
cd hack/benchmark-fixture
go run . --count 500000
```

Create a dashboard and a couple of panels, either through the web UI
(`http://localhost:3000/dashboards` → "+ Create" → "+ Add panel") or via
`cairnobsctl`:

```sh
cairnobsctl dashboards apply my-dashboard.json   # shape = GET /dashboards/{id}/export
```

**Verified live**: a table panel (`severity=INFO | head 10`) and a bar
chart panel (`| stats count by host`, viz_type `bar`) both render
correctly against real data, including drag/resize via the gridstack
grid.

### Real bugs this caught

Building and actually loading the dashboard UI against a live stack
(not just unit tests) found four real bugs, all now fixed:

1. **A Phase 2 bug, only surfaced now**: `earliest=`/`latest=` time-range
   queries never actually worked against live ClickHouse. The executor
   formatted `TimeRange` bounds with `time.RFC3339Nano`
   (`2026-08-12T20:17:40.223505479Z`), but ClickHouse's *implicit*
   string→`DateTime64` cast for a column comparison is strict and wants
   `'YYYY-MM-DD HH:MM:SS[.fractional]'` — no `T`/`Z`. Failed with `code:
   53, Cannot convert string ... to type DateTime64(9, 'UTC')`. Nothing
   in Phase 2's own runbook queries or unit tests happened to exercise a
   relative time filter live; the unit test asserted the broken format
   without ever asking real ClickHouse whether it was valid. Fixed in
   `api/internal/querylang/executor/sql.go` (`formatClickHouseDateTime64`).
2. **`"now"` as a literal query token**: the dashboard time-range picker
   injected `latest=now` verbatim; the query language has no `now`
   keyword (only quoted absolute timestamps or `-N unit` relative
   offsets). Fixed by treating `"now"` as a UI-only sentinel that omits
   the `latest=` clause entirely (`web/src/lib/api.ts`'s
   `injectTimeRange`).
3. **A layout-timing race**: GridStack/uPlot measured a panel's width
   before the grid had finished laying out, producing a 74px-wide chart
   canvas in a 540px container. Fixed with a `ResizeObserver` in
   `PanelViz.svelte` that re-renders whenever the container's actual
   size changes (also fixes chart sizing after a manual panel resize).
4. **`Date.parse` is too lenient to use as a "is this a timestamp"
   check**: `Date.parse("host-06")` returns a real (bogus) timestamp
   rather than `NaN`, which silently misrouted a categorical `stats
   count by host` column onto a numeric time axis and rendered
   unreadable giant tick labels instead of host names. Fixed by requiring
   a strict ISO-8601 prefix match before attempting `Date.parse`
   (`PanelViz.svelte`'s `isoTimestampPrefix`).

## 3. Alerting

Bring up a local webhook receiver for testing (no real Slack/PagerDuty
needed):

```sh
docker run -d --name cairnobs-webhook-sink --network sentry_default \
  -p 9099:9099 -v $(pwd)/hack/webhook-sink:/src -w /src golang:1.25-alpine go run .
```

Create a notification target and a rule, either via the web UI
(`http://localhost:3000/alerts/new`) or directly:

```sh
curl -X POST http://localhost:8081/targets -H 'Content-Type: application/json' -d '{
  "name": "local sink", "kind": "webhook", "webhook_url": "http://cairnobs-webhook-sink:9099/"
}'

curl -X POST http://localhost:8081/rules -H 'Content-Type: application/json' -d '{
  "name": "always fires", "query": "SELECT 1", "query_language": "sql",
  "condition_type": "threshold", "comparator": "gte", "threshold_value": 1,
  "eval_interval_seconds": 30, "for_minutes": 0,
  "notification_target_id": "<target id>"
}'
```

**Verified live, through both curl and the actual web UI** (created a
rule via the `/alerts/new` form, watched it transition in the browser):
the rule transitions `ok` → `firing` on its first evaluation
(`for_minutes: 0`), the delivery log shows `firing / sent / 200`, and
`docker logs cairnobs-webhook-sink` shows the real received payload.

Also verified live: a threshold rule whose query returns **zero rows**
records `last_eval_status: "error"` with the exact expected message
(`"threshold rule query returned 0 rows, want exactly 1"`) and leaves
`state` untouched at `"ok"` — confirming fix 3/4 from
`/docs/phase-3-alerting-design.md` hold in practice, not just in the unit
tests that were written against them.

### Real bugs this caught

5. **`enabled` silently defaulted to `false`.** `rulestore.Rule.Enabled`
   is a plain `bool`; a create request that simply didn't mention
   `"enabled"` decoded it to Go's zero value (`false`) rather than the
   intended "enabled by default." A rule created this way was silently
   dead on arrival — never picked up by the evaluator's claim query, no
   error anywhere. Fixed in `alerting/internal/httpapi/handler.go` with a
   `createRuleRequest` wrapper using `Enabled *bool`: `nil` (omitted)
   means enabled; only an explicit `"enabled": false` creates a disabled
   rule. Caught by actually creating a rule through the endpoint and
   checking the response, not by inspection.

## 4. Load test (task 8)

```sh
cd hack/alert-load-test
go run . --rule-count 500 --eval-interval-seconds 60 --duration 3m30s
```

Seeds 500 rules via `alerting`'s real create API (not a direct DB
insert), each querying a different host's count over the last minute
against the 500,000-row fixture dataset from §2, with an unreachably
high threshold so rules stay `ok` — isolating evaluator/ClickHouse
scheduling throughput from delivery-worker load.

**First run, before a fix**: every one of 500 rules showed
`consecutive_errors > 0`. Root cause: the load-test tool's own query
(`host=host-01`, unquoted) hit a real, pre-existing Phase 2 lexer quirk —
an unquoted comparison value containing a literal `-` fails to parse
(`unexpected MINUS after query`). Fixed by quoting the value
(`host="host-01"`) in the load-test tool itself; not a bug worth chasing
in the query language for this runbook.

**Second run, after that fix — a real, significant finding**: zero
errors, but the observed inter-evaluation interval was a suspiciously
exact **125.0s** for every one of 320 observed intervals, against a
configured `eval_interval_seconds: 60`. Root cause: `evaluator.tick()`
passed `workerPoolSize` (default 20) as *both* the claim batch size and
the concurrency limit — two genuinely different concerns conflated into
one number. With 500 rules all due at nearly the same instant (created
within ~65ms of each other), each 5-second tick could claim only 20 of
them regardless of how many more were already due, so draining the full
backlog took 500 ÷ 20 = 25 ticks × 5s = **125s** — more than double the
configured interval. Fixed by separating `EVALUATOR_CLAIM_BATCH_SIZE`
(default 1000 — how many due rules one tick can pull off the queue) from
`EVALUATOR_WORKER_POOL_SIZE` (default 20 — bounded concurrent `/query`
calls within that batch); see `alerting/internal/config/config.go`.

**Third run, after the fix**:

| Metric | Value |
|---|---|
| Rules seeded | 500 |
| Rules with `consecutive_errors > 0` | 0 |
| Configured `eval_interval_seconds` | 60 |
| Observed interval — min | 59.9s |
| Observed interval — mean | 63.3s |
| Observed interval — p95 | 65.0s |
| Observed interval — max | 65.2s |
| Drift vs. configured (p95 − configured) | 5.0s |

The remaining ~5s of "drift" is attributable to the load test's own
5-second poll granularity (an evaluation can only be observed to within
one poll interval), not to the evaluator falling behind — 500 rules at
60s intervals is comfortably within the evaluator's capacity once the
claim-batch/worker-pool conflation was fixed.

**Phase 4+ scaling paths, named but not solved here**: the claim query's
`SELECT ... FOR UPDATE SKIP LOCKED` design (see
`/docs/phase-3-alerting-design.md`'s fix 1) is specifically what makes
horizontal evaluator replicas safe to add later without a state-model
redesign — each replica's claim naturally excludes rows another replica
already claimed. Moving off a single-process ticker to a distributed
scheduler, and materially larger rule counts (10,000+), are both
explicitly out of scope for this phase.

## 5. Confirm `cairnobsctl`

```sh
cairnobsctl dashboards list
cairnobsctl dashboards apply exported-dashboard.json
cairnobsctl alerts list
cairnobsctl alerts apply rule.json
```

Both `dashboards` and `alerts` hit the exact same REST endpoints the web
UI does — `dashboards` against `/api` (`--api`), `alerts` against
`/alerting` (`--alerting-api`) — no separate CRUD logic to drift out of
sync.

## Tearing down

```sh
docker compose down -v   # wipes the fixture dataset and all dashboards/rules
```

## Troubleshooting

**A relative `earliest=`/`latest=` query fails with `code: 53, Cannot
convert string ... to type DateTime64`.**
You've reverted or bypassed `formatClickHouseDateTime64` in
`api/internal/querylang/executor/sql.go` — ClickHouse's implicit
string→`DateTime64` cast needs `'YYYY-MM-DD HH:MM:SS[.fractional]'`, not
an ISO-8601/RFC3339 literal. See §2, bug 1 above.

**A dashboard chart panel renders with a tiny or zero-width canvas.**
Check that `PanelViz.svelte`'s `ResizeObserver` is still wired up — this
is the exact symptom of the gridstack/uPlot layout-timing race from §2,
bug 3.

**A rule created via the API never evaluates, but no error appears
anywhere.**
Check the response's `"enabled"` field — if the create request omitted
`"enabled"` entirely and the response shows `false`, the
`createRuleRequest` fix in `alerting/internal/httpapi/handler.go` has
regressed. See §3, bug 5.

**500+ rules at a short `eval_interval_seconds` fall behind the
configured interval.**
Check `EVALUATOR_CLAIM_BATCH_SIZE` hasn't been set equal to (or below)
`EVALUATOR_WORKER_POOL_SIZE` — that's the exact regression `hack/alert-
load-test` caught in §4. Claim batch size should stay well above the
worker pool size; the worker pool is what bounds concurrent `/query`
load, not the claim.

**`hack/alert-load-test` reports errors on every rule.**
Check the tool's generated query quotes any comparison value that might
contain a `-` (e.g. `host="host-01"`, not `host=host-01`) — an unquoted
value with a literal hyphen fails to parse. See §4's first-run finding.
