# Alerting design

> **Status:** Design only — no code written against this yet. Task 4 of
> Phase 3. Per explicit instruction, execution stops here for review
> before task 5 (building `/alerting`) begins: the firing/resolved state
> machine and debounce behavior are easy to get subtly wrong, and this is
> the artifact to sign off on before anything is built against it. If
> implementation later reveals this design is wrong somewhere, fix this
> doc in the same change — same discipline as
> `/docs/query-language-design.md` and
> `/docs/phase-3-dashboard-design.md`.

## Why this design, in one paragraph

A rule is a saved Phase 2 query plus a condition (threshold or absence)
plus an evaluation interval plus a notification target. The genuinely
hard part isn't the data model — it's the firing/resolved state machine
under concurrent evaluation and its interaction with notification
delivery, where a naive implementation can plausibly double-fire, lose a
resolve, or silently misreport an infrastructure outage as "all clear."
This doc's state machine mirrors Prometheus Alertmanager's well-understood
`pending`/`firing` `for:` model, then adds four specific correctness
properties on top of it — not because the base model is wrong, but
because the *concurrent, at-least-once* environment a Go ticker-based
evaluator actually runs in exposes gaps a single-threaded description of
the model glosses over. Each fix below is stated as: the failure it
prevents, and the mechanism, so it's reviewable as a claim rather than
just an assertion.

## Data model

A rule is a saved query, evaluated on an interval, checked against a
condition, with a debounce before it's allowed to notify, and one
notification target it notifies. Two condition types:

- **`threshold`**: the query's result must resolve to exactly one row;
  the value in that row's first column is compared against
  `threshold_value` via `comparator`.
- **`absence`**: the query returned zero rows. The evaluation *window*
  is not a separate rule field — it's whatever `earliest=`/`latest=` the
  rule's own saved query already expresses (e.g. `service=payments
  severity=ERROR earliest=-5m`), reusing Phase 2's time-range syntax
  rather than inventing a second one.

```sql
CREATE TABLE notification_targets (
    id               UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('webhook', 'slack', 'pagerduty')),
    webhook_url      TEXT NOT NULL,
    payload_template TEXT,            -- generic ("webhook") targets only; NULL for slack/pagerduty
    headers          JSONB NOT NULL DEFAULT '{}',
    secret           TEXT,            -- PagerDuty routing key / generic-webhook HMAC secret -- plaintext, see "Known gaps" below
    created_by       TEXT NOT NULL DEFAULT 'anonymous',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE alert_rules (
    id                        UUID PRIMARY KEY,
    tenant_id                 TEXT NOT NULL DEFAULT 'default',
    name                      TEXT NOT NULL,
    description               TEXT NOT NULL DEFAULT '',
    query                     TEXT NOT NULL,
    query_language            TEXT NOT NULL DEFAULT '',
    condition_type            TEXT NOT NULL CHECK (condition_type IN ('threshold', 'absence')),
    comparator                TEXT CHECK (comparator IN ('gt', 'gte', 'lt', 'lte', 'eq', 'ne')), -- NULL for absence
    threshold_value           DOUBLE PRECISION,                                                  -- NULL for absence
    eval_interval_seconds     INT NOT NULL CHECK (eval_interval_seconds >= 30),
    for_minutes               INT NOT NULL DEFAULT 0,      -- 0 = fire on first true evaluation
    renotify_interval_minutes INT,                          -- NULL = notify once per firing episode
    notification_target_id    UUID NOT NULL REFERENCES notification_targets(id),
    enabled                   BOOLEAN NOT NULL DEFAULT true,
    created_by                TEXT NOT NULL DEFAULT 'anonymous',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE alert_state (
    rule_id              UUID PRIMARY KEY REFERENCES alert_rules(id) ON DELETE CASCADE,
    state                TEXT NOT NULL DEFAULT 'ok' CHECK (state IN ('ok', 'pending', 'firing')),
    condition_true_since TIMESTAMPTZ,
    fired_at             TIMESTAMPTZ,
    last_notified_at     TIMESTAMPTZ,
    last_evaluated_at    TIMESTAMPTZ,
    last_eval_status     TEXT NOT NULL DEFAULT 'ok' CHECK (last_eval_status IN ('ok', 'error')),
    last_error           TEXT,
    last_value           DOUBLE PRECISION,
    consecutive_errors    INT NOT NULL DEFAULT 0,
    next_eval_at         TIMESTAMPTZ NOT NULL,   -- the claim column, see "Concurrency" below
    claimed_at           TIMESTAMPTZ
);

CREATE TABLE delivery_log (
    id                     BIGSERIAL PRIMARY KEY,
    rule_id                UUID NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    notification_target_id UUID NOT NULL REFERENCES notification_targets(id),
    event_type             TEXT NOT NULL CHECK (event_type IN ('firing', 'resolved')),
    status                 TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'retrying')),
    attempt_count          INT NOT NULL DEFAULT 0,
    max_attempts           INT NOT NULL DEFAULT 5,
    next_attempt_at        TIMESTAMPTZ,   -- the delivery worker's own claim key
    last_attempt_at        TIMESTAMPTZ,
    last_error             TEXT,
    response_status        INT,
    payload                JSONB NOT NULL,  -- the actual rendered payload -- needed to debug template issues
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON delivery_log (rule_id, created_at DESC);                              -- per-rule delivery log UI
CREATE INDEX ON delivery_log (status, next_attempt_at) WHERE status IN ('pending', 'retrying');  -- delivery worker's claim query
```

**`alert_state` must be inserted in the same transaction as its owning
`alert_rules` row**, `state = 'ok'`, `next_eval_at = now()` (evaluate
immediately on creation). A rule with no `alert_state` row is silently
never picked up by the claim query below — worth stating loudly since
it's the kind of thing that "just works" in every test that remembers to
seed both rows and then silently doesn't in production the one time it's
forgotten.

Single notification target per rule for MVP. Multi-target (e.g. page AND
Slack) is a disclosed, straightforward future extension — a join table,
not a redesign.

## Notification delivery: generic webhook as the base primitive

All three `notification_targets.kind` values ultimately do the same
thing — an HTTP POST to `webhook_url` — with kind-specific *payload
formatting only*, per the explicit requirement that this stays pluggable
rather than accumulating per-vendor delivery logic:

- **`webhook`**: `payload_template` is a Go `text/template` string,
  rendered against the firing/resolved event (rule name, condition,
  current value, timestamp). No template = a sane default JSON shape.
- **`slack`**: fixed formatter producing Slack's incoming-webhook shape
  (`{"text": "..."}`), no user template. `payload_template` is ignored
  for this kind (kept nullable in the schema rather than removed, so a
  target's `kind` can be changed later without a payload column migration).
- **`pagerduty`**: fixed formatter producing PagerDuty's Events API v2
  shape (`{"routing_key": secret, "event_action": "trigger"|"resolve",
  "payload": {...}}`) — `secret` here is the PagerDuty integration
  routing key, not a delivery credential in the auth sense.

All three go through the exact same HTTP POST + retry/backoff mechanism
in `internal/delivery/webhook.go`; `slack.go`/`pagerduty.go` are payload
builders only, never their own delivery path.

## The state machine

States per rule: `ok` → `pending` → `firing`, tracked in `alert_state`.
Every evaluation produces one of three outcomes — `condition_true`,
`condition_false`, or `error` — and error is handled entirely separately
from the other two (see fix 3).

**On `condition_true`:**
- `ok` → `pending`: set `condition_true_since = now()`. No notification.
- `pending` → `firing`, once `now() - condition_true_since >=
  for_minutes`: send a **firing** notification, set `fired_at =
  last_notified_at = now()`. `for_minutes = 0` means this transition
  happens on the very first true evaluation.
- `pending`, not yet past `for_minutes`: no transition, no notification.
- `firing` → stays `firing`: silent, unless `renotify_interval_minutes`
  is set and `now() - last_notified_at >= renotify_interval_minutes`, in
  which case re-send **firing** and update `last_notified_at`. Default
  (`NULL`) is "notify once per firing episode, stay silent until
  resolved" — stated explicitly since it's exactly the kind of default an
  implementer would otherwise have to guess.

**On `condition_false`:**
- `ok` → no-op.
- `pending` → `ok`: clear `condition_true_since`. **No notification** —
  this was a blip inside the debounce window, not a real alert. This is
  deliberate, not an oversight: it's the entire reason `for_minutes`
  exists.
- `firing` → `ok`: clear `condition_true_since`/`fired_at`, send a
  **resolved** notification.

`condition_true_since` is a **wall-clock timestamp**, not a
consecutive-evaluation counter. This is what makes the debounce survive
evaluator restarts/downtime correctly: if the evaluator is down for part
of a rule's `for_minutes` window and comes back, wall-clock math
correctly resumes toward firing where it left off, while a counter would
have silently lost that progress and restarted the count. Worth
defending explicitly here since a counter looks like the "simpler"
choice and a future contributor might "simplify" it into one.

**Disclosed non-goal: no debounce on the way down.** `firing` → `ok`
happens on a single false evaluation — there's no symmetric "stay firing
for N more minutes" hold (Grafana calls this "keep firing for"). A
condition that flickers right at the threshold produces a firing/resolved
notification pair per flicker. Future work, not solved in Phase 3.

### Four correctness properties, and the concrete failure each one prevents

**1. Concurrent evaluation of the same rule (claim-then-evaluate).**
A worker-pool evaluator is required at the scale task 8 targets (~500
rules @ 60s ≈ 8+ evaluations/sec sustained). If a single evaluation's
round-trip to `api`'s `/query` ever takes longer than the rule's own
interval, the next scheduler tick can pick the *same* rule again while
the first evaluation is still in flight — two goroutines then read-
modify-write the same `alert_state` row concurrently. Depending on
timing, that produces either a duplicated firing notification (both see
`pending`, both compute "elapsed ≥ for_minutes", both fire) or a lost
resolve (a slow evaluation's stale write clobbers a faster one).

Fix: atomically claim due rules **before** the slow network call starts:

```sql
UPDATE alert_state
SET next_eval_at = now() + (eval_interval_seconds || ' seconds')::interval,
    claimed_at = now()
FROM alert_rules
WHERE alert_state.rule_id = alert_rules.id
  AND alert_state.next_eval_at <= now()
  AND alert_rules.enabled
LIMIT $batch_size
RETURNING alert_state.rule_id, alert_state.state, alert_state.condition_true_since, ...
```

`next_eval_at` is bumped *before* the `/query` HTTP call ever starts, so
a second scheduler tick can't re-select the same rule while the first is
still running. The `/query` call itself happens **outside** any database
transaction — never hold a Postgres connection open across a network
call to another service. A second, short transaction applies the
resulting state transition once the query result is known.

This same claim pattern is what makes horizontal evaluator replicas safe
to add later (a named Phase 4+ path, see task 8) without redesigning the
state model — each replica's claim query naturally excludes rows another
replica already claimed. Worth stating positively: this is the one place
in Phase 3 that's actively built *for* that future need, not just
avoiding a trap.

**2. Notification loss/duplication on crash (transactional outbox).**
If the state transition and the webhook POST happen as separate,
sequentially-ordered steps, a crash between them is unrecoverable in one
direction or the other: crash after a successful POST but before the DB
commit → next evaluation replays the transition and double-fires; crash
after commit but before the POST → the notification is silently owed
forever with no record that it was ever decided.

Fix: the state transition and `INSERT INTO delivery_log (..., status =
'pending')` happen in the **same database transaction** — "we decided to
notify" becomes durable exactly once, atomically with the state change
itself, before any network call to a notification target is attempted. A
**separate** delivery worker polls `delivery_log WHERE status IN
('pending', 'retrying') AND next_attempt_at <= now()` (same claim
pattern as fix 1), performs the actual HTTP POST, and updates
`status`/`attempt_count`/`last_error`. This decouples "did we decide to
notify" (transactionally certain) from "did the HTTP call succeed"
(best-effort with retries) — which is exactly what task 5's retry-with-
backoff requirement needs anyway, so this isn't extra machinery bolted
on for correctness's sake, it's the same piece of work.

**3. Query errors must never be treated as `condition_false`.**
The state machine above only describes `condition_true`/
`condition_false` — what happens when the `/query` call to `api` itself
fails (timeout, ClickHouse down, a 5xx)? Coercing an error to "false" is
the tempting default and the worst possible one: a `firing` alert would
silently auto-resolve and go quiet at precisely the moment something is
broken enough that the query can't even run. Coercing to "true" is
equally wrong the other way (spurious pages on transient infra hiccups).

Fix: evaluation outcome is modeled as a three-way result, and an `error`
outcome **never transitions `state`** at all. It only updates
`alert_state.last_evaluated_at`, `last_eval_status = 'error'`,
`last_error`, and increments `consecutive_errors`. This must be explicit
in the implementation, not left to the "obvious" path — the obvious
path (an error propagating into whatever boolean the rest of the
function expects) is also the wrong one.

**4. Zero rows on a `threshold` rule is an error, not a `0`.**
`stats count by host` can legitimately return zero rows for a threshold
rule (nothing matched in the window) — that's different in kind from an
`absence` rule, where zero rows *is* the signal being checked for. If a
threshold evaluation coerces "no rows" to a scalar `0` for comparison,
`count > 100` silently reports "definitely fine" in exactly the case
where the honest answer is "the query returned nothing, which might mean
nothing happened, or might mean something upstream is broken" — often
the more alarming possibility, not the safe one.

Fix, stated as an explicit rule rather than left implicit: `threshold`
evaluation requires **exactly one** result row (first row, first numeric
column, per the dashboard/query design's single-row precedent). Zero
rows — or more than one — is treated the same as fix 3's evaluation
error, not coerced to a value. Named non-goal alongside "no per-group
alerting": a threshold rule's query must resolve to a scalar.

## Evaluator architecture

A single Go process (`/alerting`), ticker-driven — **not** a workflow
engine, per the explicit instruction not to reach for one at this stage.
Loop shape:

1. Every few seconds, run the claim query (fix 1) for due, enabled rules
   up to a bounded batch size.
2. Dispatch claimed rules to a bounded worker pool (goroutines).
3. Each worker: `POST /query` against `api` (reusing the existing
   endpoint — the same precedent `cairnobsctl query` already set, never a
   second query-execution path), evaluate the condition, run the state
   transition (fix 3/4 aware) and, if applicable, the fix-2 transactional
   outbox insert, in one short DB transaction.
4. A separate delivery-worker loop claims and sends `delivery_log` rows
   independently of the evaluation loop.

`alerting` is therefore hard-dependent on `api` being reachable — if
`api`/ClickHouse is down, every due rule's evaluation records an error
(fix 3), not a false resolve. This is documented behavior, not an
accident: you can't trust a condition you can't evaluate. `alerting`'s
docker-compose entry depends on `api`'s healthcheck (added in task 3)
for this reason.

## Component boundary

`/alerting` (new top-level Go service, own `go.mod`) owns rule CRUD,
notification-target CRUD, the delivery-log read endpoint, the evaluator
loop, and the delivery worker — all four pieces of "alerting" in one
service, since they share the same Postgres tables and the same
claim-based concurrency pattern. It does **not** import `api`'s
`querylang` package or talk to ClickHouse/Tantivy directly; it only
calls `api`'s `POST /query` over HTTP, exactly like `cairnobsctl query`
and the web UI's dashboard panels already do. `web` gets a second
backend base URL (`alerting`'s) alongside the existing `api` one.

## Known gaps (named, not hidden)

- **`notification_targets.secret` is stored plaintext** in Postgres —
  the same posture `dashboard-design.md` already named for that domain.
  Becomes both an enterprise-tier (secrets/KMS) and a multi-tenancy
  (per-tenant secret isolation) concern later; naming it now avoids it
  being "discovered" as a surprise security-review finding during a
  future push.
- **CORS on `alerting`'s HTTP API is wide open**, matching `api`'s
  existing no-auth-system posture from Phases 0–2. Not a new problem
  Phase 3 introduces, just a second surface that inherits the same one.
- **No per-group/multi-row threshold alerting** (fix 4's scope decision)
  and **no resolved-side debounce** (state-machine section) are both
  named future work.
- **Single shared Postgres role** for `api` and `alerting`, same as
  `dashboard-design.md`'s note — fine for Phase 3, a named Phase 4 item
  once real auth exists.

## Load-testing plan (task 8, not run yet)

Seed ~500 rules via `alerting`'s own create API (not a direct DB insert
— exercise the real code path) at 60-second intervals, against a real
ClickHouse dataset (reusing `hack/benchmark-fixture`'s generator).
Measure: drift between `alert_state.next_eval_at` and the actual claim
timestamp under sustained load, `consecutive_errors`/`last_eval_status`
distribution (did the evaluator start erroring under load, as opposed to
just running slow), and `delivery_log.attempt_count`/`status`
distribution. Document real numbers, not projections — matching every
prior phase's benchmark discipline — and name what would need to change
for materially larger rule counts (horizontal evaluator replicas
partitioning by rule-id hash, which fix 1's claim design already makes
safe to add without a state-model change; moving off a single-process
ticker) as explicit Phase 4+ scope, not solved here.
