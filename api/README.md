# api

Cairn OBS's query API: a single `POST /query` endpoint accepting either the
pipe syntax or raw SQL, compiled and routed across ClickHouse and Tantivy
by `internal/querylang`. Replaces Phase 0/1's two separate placeholder
endpoints (raw-SQL-only `/query`, free-text-only `/search`) — see
`/docs/query-language-design.md` for the grammar, IR, and routing design,
and `/docs/query-language-reference.md` for the user-facing syntax.

## Why plain REST, not gRPC + REST gateway

CLAUDE.md pins the control plane to "Go, gRPC + REST gateway." This
service is plain `net/http` instead — a deliberate simplification, not a
change to the pinned stack. Wiring up a `.proto` service,
`google.api.http` annotations, and `protoc-gen-grpc-gateway` codegen for
one endpoint doesn't buy much at this size. `api` *does* speak gRPC
internally — to `/search` — this simplification is about the
public-facing surface only.

## Endpoint

`POST /query` — body `{"query": "...", "language": ""}`, response
`{"columns": [...], "rows": [[...], ...]}` or `{"error": "..."}`.

- `query` is either pipe syntax (`service=api | where status>=500 |
  stats count by host`) or raw SQL (`SELECT ...`). Auto-detected by
  whether the query starts with `SELECT` (case-insensitive).
- `language` optionally overrides detection: `"sql"` or `"spl"`. Exists
  for the rare case a pipe query legitimately starts with the literal
  word "select" as a bare search term.
- Both syntaxes compile to the same `querylang/ir.Plan` and execute
  through the same code path — see `internal/querylang/executor` for the
  four routing cases (pure ClickHouse; Tantivy prefilter + ClickHouse
  rows; Tantivy prefilter + ClickHouse aggregation; raw SQL passthrough).

`GET /healthz` — for docker-compose/k8s liveness checks.

No auth. Not scoped yet — don't expose this beyond a trusted dev/homelab
network.

## Configuration

Environment variables (see `internal/config/config.go`):

| Var | Default | Purpose |
|---|---|---|
| `HTTP_LISTEN_ADDR` | `:8080` | |
| `CLICKHOUSE_ADDR` | `localhost:9000` | Native protocol port |
| `CLICKHOUSE_DATABASE` / `_USERNAME` / `_PASSWORD` | `cairnobs` / `default` / `` | |
| `SEARCH_GRPC_ADDR` | `localhost:50052` | Must match `/search`'s `GRPC_LISTEN_ADDR` |
| `QUERY_TIMEOUT_SECONDS` | `30` | Per-request timeout |
| `CORS_ALLOWED_ORIGIN` | `*` | Wide open by default since there's no auth yet; tighten together |

`searchclient.Dial` connects to `/search` over plain TCP, no TLS — same
trust boundary as `api`'s existing plain-TCP connection to ClickHouse.
mTLS in this project is specifically the agent↔ingest edge boundary, not
every internal hop.

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

```sh
# from the repo root, not api/
docker build -f api/Dockerfile -t cairnobs-api .
```

## Testing notes

`internal/queryapi`'s HTTP handler depends on ClickHouse and `/search`
only through the narrow interfaces `querylang/executor` defines
(`SQLRunner`, `SearchClient`), so routing, compilation, JSON encoding,
and error-status mapping are all unit-tested against fakes — no live
ClickHouse or `/search` instance needed, and the real lexer/parser/
planner run unmocked in these tests, only the backends are faked. See
`internal/querylang`'s own package docs for how compilation and
execution are tested independently of each other. `executor.ChRunner`
(the reflection-based row scanning against ClickHouse's `driver.Rows`)
and `internal/searchclient`'s actual gRPC dial are not unit-tested — the
former because faking `driver.Rows` fully would be significant
test-only scaffolding the driver's own docs say isn't meant to be
implemented by adopters; the latter because it's a thin wrapper with
nothing but wiring to test. Both are exercised end-to-end via the
docker-compose flow in `/docs/phase-2-runbook.md`.
