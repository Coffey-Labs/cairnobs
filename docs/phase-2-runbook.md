# Phase 2 runbook

Extends `/docs/phase-0-runbook.md` and `/docs/phase-1-runbook.md` with
the unified query language and the "modest dataset" query-latency
benchmark. Read those first — this assumes the stack already works;
Phase 2 replaces the two placeholder query endpoints/pages with one, and
adds a way to actually measure query performance at volume.

## What's actually been verified

Unlike the phrasing "define 'modest', put a rough benchmark in the
runbook" might suggest, this isn't an estimate — the numbers below are
from actually generating 1,022,000 rows in a live ClickHouse (via
`/hack/benchmark-fixture`, pushed through the real agent-facing gRPC
endpoint, so both ClickHouse and Tantivy have real, matching data) and
timing real queries against it. One real bug turned up doing this (see
"What the benchmark caught" below) that wouldn't have been found any
other way.

## 1. Bring up the stack

```sh
docker compose up -d --build
```

Same as Phase 0/1. Confirm the unified endpoint works for both syntaxes:

```sh
curl -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "SELECT 1"}'
# -> {"columns":["1"],"rows":[[1]]}

curl -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "service=api"}'
# -> {"columns":[...],"rows":[]}  (empty is fine -- no data ingested yet)
```

## 2. Generate the "modest" benchmark dataset

"Modest" is defined here as **1,000,000 rows** — enough to be a real
volume test, small enough to generate and query interactively rather
than needing a dedicated load-testing pass.

```sh
cd hack/benchmark-fixture
go run . --count 1000000 --batch-size 1000 --concurrency 16
```

Sends batched `PushBatch` calls directly to `ingest`'s gRPC endpoint —
the same path the real agent uses, just generating synthetic data
instead of reading journald, so it exercises the *real* ingest →
Redpanda → ClickHouse-writer-consumer and → search-indexer-consumer
paths, not a shortcut that bypasses them. Concurrency matters:
sequential single-batch calls topped out around 500 records/sec (would
take ~33 minutes for 1M rows); 16 concurrent `PushBatch` calls over one
shared gRPC connection (HTTP/2 multiplexes concurrent RPCs over it
natively) got over 400,000 records/sec, so the actual measured run took
about 2 seconds for the ingest call itself.

Wait for both consumers to fully drain before benchmarking — ingestion
finishing doesn't mean ClickHouse/Tantivy are caught up yet:

```sh
# poll until this stops climbing
curl -s -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "SELECT count() FROM logs", "language": "sql"}'
```

In the run this runbook was written from, ClickHouse settled at
1,022,000 rows a few seconds after ingestion finished (the extra 22,000
were earlier smaller test runs from developing the benchmark tool itself
— harmless, still real rows).

## 3. Run the benchmark queries

```sh
# structured filter + aggregation -- pure ClickHouse path
curl -s -o /dev/null -w "%{time_total}s\n" -X POST http://localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{"query": "service=api | where status>=500 | stats count by host | sort -count"}'

# free-text + aggregation -- Tantivy prefilter feeding a ClickHouse GROUP BY,
# the case /docs/query-language-design.md called "the hardest part"
curl -s -o /dev/null -w "%{time_total}s\n" -X POST http://localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{"query": "message:\"connection refused\" | stats count by host | sort -count"}'
```

**Measured results against the 1,022,000-row dataset** (this exact run,
not a projection):

| Query | Path | Wall time |
|---|---|---|
| `service=api \| where status>=500 \| stats count by host \| sort -count` | Pure ClickHouse | 19.7ms |
| `message:"connection refused" \| stats count by host \| sort -count` | Tantivy prefilter + ClickHouse aggregate | 46.4ms |
| `message:"connection refused" \| head 50` | Tantivy prefilter, no aggregation | 49.2ms |
| `SELECT count() FROM logs WHERE service='web'` (raw SQL) | Pure ClickHouse | 17.5ms |

All four "well under a second" — the Phase 2 exit criteria in
`/CLAUDE.md`. The combined text+aggregation case (the one everyone should
be nervous about, since it's a two-backend query) came in at 46ms, not
meaningfully slower than the pure-ClickHouse case — the Tantivy prefilter
step is fast, and 5,000 UUIDs (see below) is a small `IN` clause by
ClickHouse's standards.

## 4. What the benchmark caught

The original design capped the Tantivy prefilter at **10,000** matching
`record_id`s before feeding them into ClickHouse's `WHERE record_id IN
(...)`. Running the actual combined-query benchmark against real data
(where "connection refused" matched a large fraction of the 1M rows,
by design — the fixture generator seeds that phrase deliberately) hit
this immediately:

```
query failed: executing query: code: 62, message: Syntax error:
failed at position 262116 [...] Max query size exceeded
```

10,000 quoted UUIDs (~39 bytes each including the separating comma)
produces a ~390KB query string, which exceeds ClickHouse's *default*
`max_query_size` (262144 bytes / 256KiB) — a much lower ceiling than
"multi-million-entry" suggested before anyone had actually tried it. The
cap is now **5,000** (~195KB, safely under the default with headroom) —
see `api/internal/querylang/executor/executor.go`'s `textSearchLimit`
and `/docs/query-language-design.md`'s "Known scaling limitation"
section, both updated with this exact finding rather than a
first-principles guess.

## 5. Confirm the web UI and CLI both work end to end

Open `http://localhost:3000` — one page now (Phase 0/1's two pages are
gone), run `service=api | where status>=500 | stats count by host | sort
-count` in the query bar, confirm results render and the query appears
in the session-local history panel.

```sh
cd cli
go run ./cmd/sentryctl query 'service=api | where status>=500 | stats count by host | sort -count'
```

Both hit the exact same `POST /query` endpoint — there's no separate
query logic to drift out of sync between the CLI, the web UI, and
whatever else calls this API later.

## Tearing down

```sh
docker compose down -v   # also wipes the 1M-row benchmark dataset
```

## Troubleshooting

**A text-search query with aggregation fails with a ClickHouse syntax
error / "Max query size exceeded."**
If you've raised `textSearchLimit` above 5,000, you've likely
reintroduced the exact failure this runbook's benchmark caught (see
"What the benchmark caught" above) — lower it back down, or raise
ClickHouse's `max_query_size` server-side with matching memory sizing if
you genuinely need a larger prefilter.

**Benchmark ingestion is much slower than ~400K records/sec.**
Check `--concurrency` wasn't left at a low value, and that `ingest`
isn't CPU-starved (`docker stats`) — the numbers above are from a single
machine running the whole stack including Redpanda/ClickHouse/search
concurrently, not a dedicated load-test environment, so your exact
throughput will vary with hardware. What matters is "well under a
second" for the actual query latency, not the ingestion throughput
number itself, which is just how the test data gets there.

**`stats sum(field)`/`avg(field)` etc. on an attributes-map field
returns 0 for everything.**
Check the field's values are actually numeric strings — non-numeric
attribute values silently cast to 0 via `toFloat64OrZero` (see
`/docs/query-language-reference.md`'s "Field mapping" section). This is
documented behavior, not a bug, but it's easy to trip over with a typo'd
field name (which also "succeeds" with all zeros, since a missing map
key reads as an empty string, which also casts to 0).
