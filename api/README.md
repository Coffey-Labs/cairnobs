# api

Sentry's Phase 0 query API: one crude, intentionally placeholder endpoint.

## Why plain REST, not gRPC + REST gateway

CLAUDE.md pins the control plane to "Go, gRPC + REST gateway." This
service is plain `net/http` instead — a deliberate Phase 0 simplification,
not a change to the pinned stack. Wiring up a `.proto` service,
`google.api.http` annotations, and `protoc-gen-grpc-gateway` codegen for a
single endpoint that Phase 2 replaces outright with a real SPL-like query
layer would be exactly the kind of premature machinery this project's
conventions warn against. Adopt the gRPC+gateway pattern once `/api` grows
a second real, durable endpoint.

## Endpoints

- `POST /query` — body `{"sql": "SELECT ..."}`, response
  `{"columns": [...], "rows": [[...], ...]}` or `{"error": "..."}`.
  SELECT-only, single-statement, basic keyword-based injection guarding
  (see `internal/queryapi/validate.go` for exactly what that does and
  doesn't catch — it's not a SQL parser).
- `GET /healthz` — for docker-compose/k8s liveness checks.

No auth. Not scoped for Phase 0 — don't expose this beyond a trusted
dev/homelab network.

## Configuration

Environment variables (see `internal/config/config.go`):

| Var | Default | Purpose |
|---|---|---|
| `HTTP_LISTEN_ADDR` | `:8080` | |
| `CLICKHOUSE_ADDR` | `localhost:9000` | Native protocol port |
| `CLICKHOUSE_DATABASE` / `_USERNAME` / `_PASSWORD` | `sentry` / `default` / `` | |
| `QUERY_TIMEOUT_SECONDS` | `30` | Per-request ClickHouse query timeout |
| `CORS_ALLOWED_ORIGIN` | `*` | Wide open by default since there's no auth yet; tighten together |

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

`internal/queryapi`'s HTTP handler depends on ClickHouse only through a
one-method `queryExecutor` interface, so routing, validation, JSON
encoding, and error-status mapping are all unit-tested against a fake —
no live ClickHouse needed. `Executor` itself (the reflection-based row
scanning against `driver.Rows`) is not unit-tested — faking ClickHouse's
`driver.Rows` interface fully would be significant test-only scaffolding
for a Phase 0 placeholder, and the driver package's own docs note it isn't
meant to be implemented by adopters. It's exercised end-to-end via the
docker-compose flow in `/docs/phase-0-runbook.md` instead.
