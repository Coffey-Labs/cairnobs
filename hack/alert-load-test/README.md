# alert-load-test

Seeds a realistic number of concurrent alert rules via `/alerting`'s real
create API (not a direct DB insert) and measures whether the evaluator's
claim scheduling keeps up under load. See
`/docs/phase-3-alerting-design.md`'s "Load-testing plan" and
`/docs/phase-3-runbook.md` for the methodology and real measured results.

```sh
# 1. Push real data so rule queries have real work to do (reuses
#    hack/benchmark-fixture):
cd ../benchmark-fixture
go run . --count 500000

# 2. Run a webhook-sink so the (never-firing, by design) rules have a
#    valid notification target to point at:
docker run -d --name sentry-webhook-sink --network sentry_default \
  -p 9099:9099 -v $(pwd)/../webhook-sink:/src -w /src golang:1.25-alpine go run .

# 3. Run the load test:
cd ../alert-load-test
go run . --rule-count 500 --eval-interval-seconds 60 --duration 3m30s
```

Each rule queries a different host's count over the last minute
(`earliest=-1m host="host-01" | stats count`) against real ClickHouse
data, with `threshold_value` set unreachably high so rules stay `ok` --
this isolates evaluator/ClickHouse scheduling throughput from
delivery-worker load (a query that never fires still exercises the exact
same claim → `/query` → evaluate → `ApplyTransition` path every tick).

The report shows, per rule, the observed intervals between consecutive
`last_evaluated_at` changes (polled at `--poll-interval`), compared
against the configured `eval_interval_seconds`. A real, significant
finding from actually running this: the evaluator's claim batch size and
worker-pool concurrency limit defaulted to the same number (20), so 500
rules all due at once took 125s to cycle through instead of the
configured 60s. Fixed by separating `EVALUATOR_CLAIM_BATCH_SIZE` from
`EVALUATOR_WORKER_POOL_SIZE` (see `alerting/internal/config/config.go`).

Cleans up the seeded rules and notification target on exit unless
`--no-cleanup` is passed.
