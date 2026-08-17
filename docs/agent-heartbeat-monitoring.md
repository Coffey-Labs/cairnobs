# Agent heartbeat and unavailability alerting

Every Linux/Windows agent (`/agent`) sends a small, independent "still
alive" record on its own schedule, in addition to whatever real log
traffic is flowing. This is what "define polling resolution in seconds,
minutes, hours" means in practice: how often an agent proves it's still
reachable, and how quickly the platform notices when it stops.

## Design: why this is a heartbeat, not a true pull

The agent's transport has always been push-only, by design — it dials
*out* to `ingest` over mTLS; nothing in the platform ever dials into an
agent (see `agent/sentry-agent/src/grpc.rs`'s doc comment). Making
liveness detection a true pull (the platform reaching into every remote
host on a schedule) would mean every agent needs a reachable address and
an open inbound port — a real problem for hosts behind NAT or with
dynamic IPs, which is the common case for a "remote" fleet, and exactly
the class of problem push was chosen to avoid.

A heartbeat gets the same outcome — the platform notices an agent going
quiet, on a configurable cadence — without any of that: the agent keeps
its one existing egress path, and "unavailable" is just the *absence* of
its heartbeat records, which the alerting engine (Phase 3) already
detects natively via `condition_type: "absence"`. No new RPC, no new
ingest code, no new ClickHouse schema, no new alert rule type — see
`/docs/phase-3-alerting-design.md` for the existing absence-condition
model this reuses unchanged.

## Configuring the heartbeat

`agent/sentry-agent/config/agent.example.toml`:

```toml
[heartbeat]
enabled = true
interval = "60s"    # or "5m", "1h" -- same s/m/h vocabulary as earliest=/latest=
```

The heartbeat record is sent through the exact same `PushBatch` RPC and
mTLS identity every log line uses, bypassing the batch buffer (`[batch]`
`max_size`/`flush_interval_ms`) so it's punctual rather than subject to
batching delay. It's distinguished from real log data purely by an
attribute — `sentry.heartbeat=true` — not by a fake `service` value, so
it never pollutes service-based dashboards or faceting. `message` is the
literal string `"agent heartbeat"`.

## Building the alert rule

There's no dedicated "agent monitor" rule type — you create an ordinary
absence alert rule, scoped to one host, with a query window a little
wider than the heartbeat interval (so an evaluation landing just after a
heartbeat doesn't look like a false absence):

```sh
curl -X POST http://localhost:8081/rules -H 'Content-Type: application/json' -d '{
  "name": "web-01 unavailable",
  "description": "fires when web-01 misses its heartbeat window",
  "query": "earliest=-3m host=web-01 sentry.heartbeat=true",
  "query_language": "spl",
  "condition_type": "absence",
  "eval_interval_seconds": 60,
  "for_minutes": 0,
  "notification_target_id": "<your notification target id>",
  "enabled": true
}'
```

- **`query`**: `earliest=-Ns/m/h` should comfortably exceed the agent's
  configured `[heartbeat] interval` — 2-3x it is a reasonable default,
  the same margin any liveness check needs against jitter.
  `host=<hostname>` scopes the rule to one specific agent — the
  evaluator's absence check only asks "did any row come back," so a
  query spanning multiple hosts would only fire when *every* host in it
  goes quiet at once, not when one specific host does (this is the same
  "no per-group/multi-row alerting" limitation `/docs/phase-3-alerting-
  design.md` already documents for threshold rules — one rule per
  resource, not a fleet-wide wildcard, is real future work, not an
  oversight here).
- **`eval_interval_seconds`**: the alerting-side "polling resolution" —
  how often this specific rule is re-checked. Already second-granular
  (any multiple of 30, the engine's documented floor — see below); a
  value in minutes or hours is just a larger number of seconds, no
  separate unit field needed.
- **`for_minutes`**: `0` fires on the very first absent evaluation, no
  debounce. A real fleet might prefer `1` or `2` to ride out a single
  missed evaluation before paging anyone — the same tradeoff any other
  absence rule makes.

**`eval_interval_seconds` has a real floor of 30**, enforced by
`alerting`'s rule-creation validation (`eval_interval_seconds must be at
least 30`) — a rule can't be checked more often than every 30 seconds
regardless of how fast the agent's own heartbeat is. An agent heartbeat
interval faster than that (this doc's own live verification used 5s) is
still useful — it tightens how quickly *evidence* of an outage
accumulates in the query window — but the alert itself can't fire on a
tighter cadence than 30s.

## Verified live

This exact flow was run end-to-end against a live stack in this repo: a
real `sentry-agent` binary, heartbeat interval 5s, connected to a real
`ingest`; an absence rule (`earliest=-45s host=... sentry.heartbeat=true`,
`eval_interval_seconds=30`, `for_minutes=0`) created via the REST API
above; the agent process killed; the rule transitioned `ok` → `firing`
within one evaluation cycle (`condition_true_since`/`fired_at` both set
at the same timestamp the query window's silence became detectable);
and a real webhook delivery landed (`delivery_log` row: `status: sent,
response_status: 200`).

**A real, independent bug was found and fixed during this
verification**: the query language's lexer never treated `-` as part of
an identifier, so any unquoted hyphenated filter value —
`host=heartbeat-test-host`, or even the reference doc's own canonical
example `host!=host-03` — failed to parse at all
(`unexpected MINUS after query`). This wasn't specific to heartbeat
monitoring; it affected any hyphenated host/service name filtered
unquoted, which is extremely common. Fixed in
`api/internal/querylang/lexer/lexer.go`'s `isIdentPart` (now includes
`-`, but only mid-identifier — a leading `-` still lexes as its own
`Minus` token, so `earliest=-1h` and `sort -count` are unaffected).
Regression tests: `TestLexIdentWithInternalHyphens`,
`TestLexFilterWithHyphenatedValue` (lexer),
`TestParseFilterWithHyphenatedValue`,
`TestParseNegativeTimeExprStillWorks` (parser).
