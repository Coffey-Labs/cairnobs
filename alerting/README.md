# alerting

Alert rule CRUD, the ticker-driven evaluator, and webhook/Slack/PagerDuty
delivery. See `/docs/phase-3-alerting-design.md` for the full design
(data model, the ok/pending/firing state machine, and the four
correctness properties this implementation follows exactly).

## Running

```sh
POSTGRES_PASSWORD=sentry-dev-only API_QUERY_URL=http://localhost:8080 go run ./cmd/alerting
```

Talks to the same `sentry_metadata` Postgres database as `/api`
(different tables — see `/metadata/README.md`), and to `/api`'s
`POST /query` over plain HTTP for rule evaluation. Never connects to
ClickHouse or Tantivy directly.

## HTTP API

```
POST   /rules                    create a rule
GET    /rules                    list rules (with current state)
GET    /rules/{id}               get a rule (with current state)
DELETE /rules/{id}
GET    /rules/{id}/deliveries    delivery log for a rule, most recent first

POST   /targets                  create a notification target
GET    /targets
GET    /targets/{id}
DELETE /targets/{id}

GET    /healthz
```

A rule's `condition_type` is `"threshold"` (requires `comparator` +
`threshold_value`, and the query must resolve to exactly one row) or
`"absence"` (fires when the query returns zero rows in its own
`earliest=`/`latest=` window — no separate window field). A notification
target's `kind` is `"webhook"`, `"slack"`, or `"pagerduty"` — all three
deliver via the same HTTP POST + retry/backoff mechanism
(`internal/delivery/webhook.go`); slack/pagerduty are payload formatters
only, not separate delivery paths.

## Environment variables

| Var | Default |
|---|---|
| `HTTP_LISTEN_ADDR` | `:8081` |
| `POSTGRES_ADDR` | `localhost:5432` |
| `POSTGRES_DATABASE` | `sentry_metadata` |
| `POSTGRES_USERNAME` | `sentry` |
| `POSTGRES_PASSWORD` | (empty — must be set) |
| `API_QUERY_URL` | `http://localhost:8080` |
| `CORS_ALLOWED_ORIGIN` | `*` |
| `EVALUATOR_TICK_SECONDS` | `5` — how often the scheduler checks for due rules |
| `EVALUATOR_CLAIM_BATCH_SIZE` | `1000` — how many due rules one tick can pull off the queue |
| `EVALUATOR_WORKER_POOL_SIZE` | `20` — bounded concurrency for `/query` calls within a claimed batch |
| `EVALUATOR_QUERY_TIMEOUT_SECONDS` | `30` — per-evaluation `POST /query` timeout |

`EVALUATOR_CLAIM_BATCH_SIZE` and `EVALUATOR_WORKER_POOL_SIZE` are
deliberately separate knobs, not the same number — see
`internal/config/config.go`'s doc comment for the real bug this
separation fixes (found by `hack/alert-load-test`, see
`/docs/phase-3-runbook.md`): with both capped at 20, 500 rules due at
once took 125s to cycle through instead of the configured 60s.

## Package layout

```
cmd/alerting/          wires config, Postgres pool, api client; runs the
                        HTTP server + evaluator + delivery worker concurrently (errgroup)
internal/httpapi/      REST handlers -- Handler/RegisterRoutes, same shape as api/internal/dashboards
internal/rulestore/    pgx CRUD for alert_rules + alert_state; ClaimDueRules
                        (fix 1's atomic claim) and ApplyTransition (fix 2's transactional outbox)
internal/notifystore/  pgx CRUD for notification_targets
internal/queryclient/  thin HTTP client to api's POST /query -- no querylang import here
internal/evaluator/    the ticker + worker pool; transitions.go is the pure,
                        exhaustively-tested ok/pending/firing state machine;
                        condition.go implements fixes 3/4 (errors never
                        coerced to "condition false"; threshold zero-rows
                        is an error, not a 0)
internal/delivery/     webhook.go is the claim-and-send worker (all three
                        kinds go through it); slack.go/pagerduty.go are
                        payload formatters only
```

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

```sh
docker build -f Dockerfile -t sentry-alerting .   # context is alerting/, not the repo root -- no /proto needed
```
