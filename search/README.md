# search

Tantivy-backed full-text search over log messages. Phase 1's answer to
"grep across everything," separate from ClickHouse's structured/
aggregation queries.

## Why a separate service, not embedded in ingest

Tantivy is a Rust library with no maintained Go bindings — using it from
`ingest` (Go) would mean cgo-bridging to a compiled Rust cdylib, exactly
the fragile FFI complexity CLAUDE.md's "prefer boring, well-understood
dependencies... operators need to trust it" principle steers away from.
It would also couple ClickHouse-write latency to Tantivy-write latency in
the same request path. See `/docs/architecture.md` for the fuller
tradeoff writeup (dual-write vs. second-consumer-group) from Phase 1
planning.

## How it fits together

```
ingest (gRPC front end) --> Redpanda (sentry.logs.raw) --> ingest's ClickHouse-writer consumer --> ClickHouse
                                    \
                                     `--> search's own consumer --> Tantivy index
```

`search` reads the *same* Redpanda topic `ingest`'s ClickHouse-writer
consumer reads, as an independent consumer in spirit (own offset
tracking, own failure domain — see "Offset tracking" below) even though
it's a completely separate process/service. If Tantivy indexing lags or
crashes, ClickHouse ingestion is completely unaffected.

`api` calls `search`'s `SearchService.Search` gRPC RPC (see
`/proto/sentry/search/v1/search.proto`) to resolve a free-text query into
matching `record_id`s, then joins those back against ClickHouse's
`logs.record_id` column (see `/storage/migrations/0002_add_record_id.sql`)
to get full rows. `search` only ever returns IDs, never row data — it
stays a pure text index, not a second copy of the row.

## Offset tracking: why this isn't a Kafka consumer group

`rskafka` (chosen for being pure Rust, no cgo — consistent with why
`ingest` chose `segmentio/kafka-go` over `confluent-kafka-go`) is a
low-level client: it doesn't implement Kafka's broker-side consumer-group
coordination protocol the way `kafka-go` or `librdkafka` do. So `search`
tracks its own per-partition offsets in a plain JSON file next to the
Tantivy index (`offsets.rs`), persisted after each fetched batch.

This is deliberately best-effort, not exactly-once: if the process dies
between processing a record and persisting its offset, that record gets
reprocessed after restart. This is safe because `SearchIndex::upsert` is
**delete-then-add on `record_id`** — Tantivy segments are immutable, so
this is the standard idiom for updates anyway, and it happens to make
reprocessing idempotent for free. Partition count is read from
`REDPANDA_TOPIC_PARTITIONS` (must match what
`/transport/provision-topics.sh` actually created — same kind of
documented cross-component contract as the topic name itself), not
discovered dynamically.

## Query syntax

Whatever Tantivy's own `QueryParser` supports against the `message`
field: plain terms, `"exact phrase"` queries, and `foo*` wildcards. Not
documented further here because it's Tantivy's syntax, not Sentry's — see
[Tantivy's query parser docs](https://docs.rs/tantivy/latest/tantivy/query/struct.QueryParser.html)
for the full grammar. No unified query language yet; that's Phase 2.

## Configuration

Environment variables (see `src/config.rs`):

| Var | Default | Purpose |
|---|---|---|
| `GRPC_LISTEN_ADDR` | `0.0.0.0:50052` | Full socket address — Rust's parser needs one, unlike Go's `:PORT` shorthand `ingest`/`api` use |
| `REDPANDA_BROKERS` | `localhost:9092` | Comma-separated broker list |
| `REDPANDA_TOPIC` | `sentry.logs.raw` | Must match `/ingest`'s topic |
| `REDPANDA_TOPIC_PARTITIONS` | `6` | Must match what `/transport/provision-topics.sh` created |
| `INDEX_PATH` | `/var/lib/sentry-search/index` | Tantivy index directory |
| `OFFSETS_PATH` | `/var/lib/sentry-search/offsets.json` | Offset tracking file |
| `COMMIT_INTERVAL_MS` | `2000` | How often buffered writes become searchable |

## Building & testing

```sh
cargo build --release
cargo clippy --all-targets -- -D warnings
cargo test
```

`index.rs`'s tests run against a real (temp-directory) Tantivy index —
no external service needed, unlike ClickHouse. They cover the
delete-then-add idempotency, phrase queries, result limits, and the
before-commit/after-commit visibility boundary. `consumer.rs` (the
rskafka wiring) is not unit-tested — that needs a real Redpanda, same
category of gap as `/ingest`'s `kafka.Reader`/`kafka.Writer` wiring, and
is exercised by the docker-compose end-to-end flow instead.

```sh
# from the repo root, not search/
docker build -f search/Dockerfile -t sentry-search .
```
