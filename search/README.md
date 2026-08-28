# search

Tantivy-backed full-text search over log messages. Phase 1's answer to
"grep across everything," separate from ClickHouse's structured/
aggregation queries.

## Why a separate service, not embedded in ingest

Tantivy is a Rust library with no maintained Go bindings — using it from
`ingest` (Go) would mean cgo-bridging to a compiled Rust cdylib, exactly
the fragile FFI complexity PROJECT-SPEC.md's "prefer boring, well-understood
dependencies... operators need to trust it" principle steers away from.
It would also couple ClickHouse-write latency to Tantivy-write latency in
the same request path. See `/docs/architecture.md` for the fuller
tradeoff writeup (dual-write vs. second-consumer-group) from Phase 1
planning.

## How it fits together

```
ingest (gRPC front end) --> Redpanda (cairnobs.logs.raw) --> ingest's ClickHouse-writer consumer --> ClickHouse
                                    \
                                     `--> search's own consumer --> Tantivy index
```

`search` reads the *same* Redpanda topic `ingest`'s ClickHouse-writer
consumer reads, as an independent consumer in spirit (own offset
tracking, own failure domain — see "Offset tracking" below) even though
it's a completely separate process/service. If Tantivy indexing lags or
crashes, ClickHouse ingestion is completely unaffected.

`api` (or, for a multi-tenant deployment, `enterprise-api` — see
`/enterprise/README.md`) calls `search`'s `SearchService.Search` gRPC RPC
(see `/proto/sentry/search/v1/search.proto`) to resolve a free-text
query into matching `record_id`s, then joins those back against
ClickHouse's `logs.record_id` column (see
`/storage/migrations/0002_add_record_id.sql`) to get full rows. `search`
only ever returns IDs, never row data — it stays a pure text index, not
a second copy of the row.

## Per-tenant indices (Phase 4) — read and write

`SearchRequest.tenant_id` (empty by default) selects which index
`src/registry.rs`'s `IndexRegistry` searches: empty resolves to the
single default index every deployment already had; a non-empty value
opens (on first use) a dedicated index under `TENANTS_INDEX_PATH`
(default `/var/lib/cairnobs-search/tenants/<tenant_id>`, matching
`deploy/operator`'s and `enterprise/internal/rbacstore`'s existing path
convention). `tenant_id` is set only by a trusted server-side caller
(`enterprise/internal/searchclient`, from the authenticated request
identity) — never a value a browser/client controls.

`consumer.rs`'s Redpanda consumer resolves the *same* registry now,
keyed by each Kafka message's `tenant_id` header — attached server-side
by `ingest/internal/grpcserver` after validating an agent's per-tenant
credential, mirroring exactly how `enterprise/cmd/enterprise-ingest`
routes the ClickHouse write side (see `/enterprise/README.md`'s "Ingest
write-routing" section). No import boundary to work around here, unlike
Go: `IndexRegistry` already lives in this AGPL-core binary, so both the
read and write paths share one registry directly, with no "second
binary" needed. The periodic Tantivy commit (`COMMIT_INTERVAL_MS`) now
commits every tenant index that's actually seen a write, plus the
default index, via `IndexRegistry::commit_all`, not just one index.

**The active-tenant gap is closed too, the same way the read side closes
it**: `src/tenants.rs`'s `ActiveTenantTracker` polls a new
`GET /internal/active-tenants` endpoint on `enterprise-auth` (this
process has no Postgres access, so unlike `enterprise/internal/
searchclient`'s `TenantChecker` — a direct `rbacstore.TenantIsActive`
call, since that code runs in `enterprise/` — this needed a network
call instead), and `consumer.rs` refuses (logs and skips, never falls
back to the default or another tenant's index) any tagged record whose
`tenant_id` isn't in the polled allowlist. Off unless
`ENTERPRISE_AUTH_URL`/`ENTERPRISE_AUTH_SERVICE_TOKEN` are both set (see
Configuration below) — when they aren't, write-routing behaves exactly
as it did before this tracker existed, trusting any syntactically-valid
`tenant_id`. When they are, startup blocks on the first fetch succeeding
(fail-closed cold start — see `tenants.rs`'s doc comment for why a
partial/degraded startup isn't the safer choice, and for the
last-known-good behavior periodic refresh failures fall back to).
Verified with real HTTP round trips against a hand-rolled TCP test
server (no mocking crate needed for one endpoint) — the Bearer token
actually sent, the initial-fetch-fails-closed path, and an unreachable
server also failing closed. See `src/registry.rs`'s doc comment on
`resolve` for how the mechanism (index lifecycle) and policy (who gets
gated) responsibilities are split.

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
documented further here because it's Tantivy's syntax, not Cairn OBS's — see
[Tantivy's query parser docs](https://docs.rs/tantivy/latest/tantivy/query/struct.QueryParser.html)
for the full grammar. No unified query language yet; that's Phase 2.

## Configuration

Environment variables (see `src/config.rs`):

| Var | Default | Purpose |
|---|---|---|
| `GRPC_LISTEN_ADDR` | `0.0.0.0:50052` | Full socket address — Rust's parser needs one, unlike Go's `:PORT` shorthand `ingest`/`api` use |
| `REDPANDA_BROKERS` | `localhost:9092` | Comma-separated broker list |
| `REDPANDA_TOPIC` | `cairnobs.logs.raw` | Must match `/ingest`'s topic |
| `REDPANDA_TOPIC_PARTITIONS` | `6` | Must match what `/transport/provision-topics.sh` created |
| `INDEX_PATH` | `/var/lib/cairnobs-search/index` | Default (non-tenant) Tantivy index directory |
| `TENANTS_INDEX_PATH` | `/var/lib/cairnobs-search/tenants` | Per-tenant index directories live under here, one subdirectory per tenant_id (Phase 4) |
| `OFFSETS_PATH` | `/var/lib/cairnobs-search/offsets.json` | Offset tracking file |
| `COMMIT_INTERVAL_MS` | `2000` | How often buffered writes become searchable |
| `ENTERPRISE_AUTH_URL` | (empty) | Enables `tenants::ActiveTenantTracker` -- empty means write-routing has no active-tenant gate, same as every deployment before Phase 4. Must be set together with `ENTERPRISE_AUTH_SERVICE_TOKEN` below, or `Config::load` fails |
| `ENTERPRISE_AUTH_SERVICE_TOKEN` | (empty) | RoleService Bearer credential for `GET /internal/active-tenants`, minted via `enterprise-auth -mint-service-token search` |

## Building & testing

```sh
cargo build --release
cargo clippy --all-targets -- -D warnings
cargo test
```

`index.rs`'s tests run against a real (temp-directory) Tantivy index —
no external service needed, unlike ClickHouse. They cover the
delete-then-add idempotency, phrase queries, result limits, and the
before-commit/after-commit visibility boundary. `registry.rs`'s tests
are the same shape and cover the actual adversarial claim: real per-
tenant indices, real documents, and a search scoped to one tenant
returning zero results for another tenant's matching document
(`tenant_index_is_isolated_from_default_and_other_tenants`), plus
`commit_all_commits_default_and_every_opened_tenant_index` proving the
write-routing commit path actually makes every tenant's buffered writes
searchable, not just the default index's — all of this, unlike almost
everything else in Phase 4, was genuinely run in the environment this
was built in, not just written. `consumer.rs`'s Kafka/rskafka wiring
itself is still not unit-tested — that needs a real Redpanda, same
category of gap as `/ingest`'s `kafka.Reader`/`kafka.Writer` wiring, and
is exercised by the docker-compose end-to-end flow instead — but the
pure `tenant_id_from_headers` header-extraction helper it uses is
factored out and unit-tested the same way `ingest/consumer`'s Go
equivalent is, including a guard test
(`test_tenant_id_header_key_matches_go`) against the header-key literal
drifting from the Go side's. `tenants.rs`'s tests genuinely exercise
`reqwest` against a real (if hand-rolled, dependency-free) TCP server —
the actual `Authorization: Bearer` header construction, JSON response
parsing, and both fail-closed paths (a rejected first fetch, an
unreachable server), not a fake HTTP client substituted in.

```sh
# from the repo root, not search/
docker build -f search/Dockerfile -t cairnobs-search .
```
