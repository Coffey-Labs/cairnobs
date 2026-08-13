# api

Sentry's query API: two intentionally crude endpoints — raw SQL and
free-text search — that Phase 2's real query layer replaces outright.

## Why plain REST, not gRPC + REST gateway

CLAUDE.md pins the control plane to "Go, gRPC + REST gateway." This
service is plain `net/http` instead — a deliberate simplification, not a
change to the pinned stack. Wiring up `.proto` services,
`google.api.http` annotations, and `protoc-gen-grpc-gateway` codegen for
two endpoints that Phase 2 replaces outright with a real SPL-like query
layer would be exactly the kind of premature machinery this project's
conventions warn against. `api` *does* speak gRPC internally though — to
`/search` (see below) — this simplification is specifically about the
public-facing surface, not a blanket avoidance of gRPC.

## Endpoints

- `POST /query` — body `{"sql": "SELECT ..."}`, response
  `{"columns": [...], "rows": [[...], ...]}` or `{"error": "..."}`.
  SELECT-only, single-statement, basic keyword-based injection guarding
  (see `internal/queryapi/validate.go` for exactly what that does and
  doesn't catch — it's not a SQL parser).
- `POST /search` — body `{"query": "...", "limit": 100}`, same response
  shape as `/query`. Calls `/search`'s `SearchService.Search` gRPC RPC to
  resolve the free-text query into matching `record_id`s, then joins
  those back against ClickHouse (`SELECT * FROM logs WHERE record_id IN
  (...)`) to return full rows — so both endpoints return the same
  `{columns, rows}` shape and `/web` can reuse one table component for
  both. Every `record_id` is validated as a real UUID before being
  embedded in the generated SQL (defense in depth: `record_id`s come from
  an internal, trusted service, not raw user input, but a value that
  fails to parse as a UUID can't contain SQL-breaking characters either
  way).
- `GET /healthz` — for docker-compose/k8s liveness checks.

No auth. Not scoped yet — don't expose this beyond a trusted dev/homelab
network.

## Configuration

Environment variables (see `internal/config/config.go`):

| Var | Default | Purpose |
|---|---|---|
| `HTTP_LISTEN_ADDR` | `:8080` | |
| `CLICKHOUSE_ADDR` | `localhost:9000` | Native protocol port |
| `CLICKHOUSE_DATABASE` / `_USERNAME` / `_PASSWORD` | `sentry` / `default` / `` | |
| `SEARCH_GRPC_ADDR` | `localhost:50052` | Must match `/search`'s `GRPC_LISTEN_ADDR` |
| `QUERY_TIMEOUT_SECONDS` | `30` | Per-request timeout, both endpoints |
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
docker build -f api/Dockerfile -t sentry-api .
```

## Testing notes

`internal/queryapi`'s HTTP handlers depend on ClickHouse and `/search`
only through narrow interfaces (`queryExecutor`, `searchClient`), so
routing, validation, JSON encoding, error-status mapping, and the
record_id-to-SQL query building are all unit-tested against fakes — no
live ClickHouse or `/search` instance needed. `Executor` itself (the
reflection-based row scanning against `driver.Rows`) and
`internal/searchclient`'s actual gRPC dial are not unit-tested — the
former because faking ClickHouse's `driver.Rows` interface fully would be
significant test-only scaffolding the driver's own docs say isn't meant
to be implemented by adopters; the latter because it's a thin wrapper
with nothing but wiring to test. Both are exercised end-to-end via the
docker-compose flow in `/docs/phase-1-runbook.md` instead.
