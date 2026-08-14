# enterprise

**Commercial license, not AGPLv3** — see `/CLAUDE.md`'s licensing
boundary. SSO (OIDC/SAML), tenant provisioning, and RBAC. Nothing in
`/agent`, `/ingest`, `/storage`, `/api`, `/web` core, or `/cli` imports
from this module — confirmed by `hack/check-tenant-boundary.sh`, run in
CI. `enterprise/` supplies tenant-scoped implementations of core's
already-shipped `api/internal/querylang/executor.SQLRunner`/
`SearchClient` interfaces rather than core growing tenant awareness —
see `/docs/phase-4-isolation-design.md` for why.

## Status

Tasks 3-5 (module skeleton, SSO library wiring, audit logging, and auth
wiring in `/api`/`/web`/`/cli`) are built and tested. What's live
end-to-end:

- `internal/session` issues/validates signed (HS256/JWT) tokens for both
  human sessions and `/alerting`'s `RoleService` credential.
- `internal/authhandler` serves `POST /internal/authorize` (the endpoint
  `api/internal/authz.HTTPAuthorizer` calls) and `GET /auth/features`
  (the runtime-capability check `/web`'s settings page reads).
- `api`'s `/query` and `/dashboards` endpoints enforce RBAC via
  `authz.RequireRole`/`RequireRoleOrService`, nil-safe (no-op) when
  `ENTERPRISE_AUTH_URL` isn't configured -- matches Phase 0-3 behavior.
- `/alerting`'s `queryclient` presents a `RoleService` Bearer token
  (`API_SERVICE_TOKEN`) when configured -- see
  `/docs/phase-4-isolation-design.md`'s `alerting`↔`api` gap.
- `sentryctl` presents `$SENTRYCTL_TOKEN` as a Bearer credential on every
  request when set.
- `internal/rbacstore`: full CRUD over `users`/`tenants`/
  `tenant_memberships` (`metadata/migrations/0017-0023`), verified
  against a live Postgres.

**Deliberately deferred, not half-built** -- named explicitly rather than
silently left out:
- The actual OIDC/SAML login/callback HTTP handlers that would issue a
  *human* session after a real IdP round trip (`internal/oidc`/
  `internal/saml` do the protocol mechanics; nothing calls them from an
  HTTP handler yet). `-mint-service-token` is the only way to get a
  token today, and it only mints `RoleService` credentials.
- `dashboard_permissions`/`data_sources` CRUD (schema exists,
  `metadata/migrations/0024-0026`; no caller reads per-resource grants
  yet -- `dashboards`' handler enforces tenant-baseline role only, not
  the matrix's "(own/granted)" qualifier).
- `internal/tenantprovision` (ClickHouse DB/user/grant + Tantivy index
  provisioning) and the tenant-scoped `internal/chrunner`/
  `internal/searchclient` `SQLRunner`/`SearchClient` implementations --
  task 2's isolation model, not yet built against real per-tenant
  connections.
- Wiring `internal/audit` into `api`'s `queryapi.AuditLogger` extension
  point (built in core since task 4, still passed as `nil`).

## Package layout

```
cmd/enterprise-auth/   config loading, OIDC discovery at startup, health/authorize/features endpoints, -mint-service-token
internal/tenant/        the ID type -- see its package doc comment before touching it
internal/oidc/           coreos/go-oidc wiring: discovery, login redirect, code exchange + ID token verification
internal/saml/            crewjam/saml wiring: SP setup, login redirect, response parsing/validation
internal/session/          issues/validates signed session + RoleService tokens
internal/authhandler/       POST /internal/authorize, GET /auth/features
internal/rbacstore/          users/tenants/tenant_memberships CRUD (pgx against sentry_metadata)
internal/audit/            append-only, hash-chained query audit log -- see its own package
                            doc comment and /docs/phase-4-isolation-design.md's audit section
internal/config/          env-var config, same convention as every other Go service here
```

Future additions: `internal/tenantprovision`, `internal/chrunner`/
`internal/searchclient` (tenant-scoped `SQLRunner`/`SearchClient`
implementations), the OIDC/SAML login/callback HTTP handlers, and
`dashboard_permissions`/`data_sources` CRUD -- see "Status" above.

## Why OIDC and SAML aren't hand-rolled

`coreos/go-oidc` (built on `golang.org/x/oauth2`) and `crewjam/saml`
handle token/assertion signature verification, XML signing, and the
protocol-level trust establishment — exactly the parts of an SSO
integration where a from-scratch implementation is the highest-risk
code in the whole feature. Both are well-established libraries, matching
this project's existing "boring, well-understood dependency" pattern
(`clickhouse-go/v2`, `jackc/pgx/v5`).

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

`internal/audit`'s real guarantees (the `audit_writer` grant
restriction, the immutability trigger, hash-chain correctness under
concurrency) can only be proven against a real Postgres — those
integration tests are skipped by default and only run with
`AUDIT_TEST_POSTGRES_ADDR` set:

```sh
docker run --rm --network sentry_default -v $(pwd)/..:/src -w /src/enterprise \
  -e AUDIT_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e AUDIT_TEST_POSTGRES_PASSWORD=audit-writer-dev-only \
  -e AUDIT_TEST_ADMIN_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/audit/... -v
```

`internal/rbacstore`'s tests are the same shape (real SQL, real
constraints), skipped unless `RBACSTORE_TEST_POSTGRES_ADDR` is set:

```sh
docker run --rm --network sentry_default -v $(pwd)/..:/src -w /src/enterprise \
  -e RBACSTORE_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e RBACSTORE_TEST_POSTGRES_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/rbacstore/... -v
```

## Turning on auth enforcement for manual testing

Off by default (see "Status" above -- there's no login flow to issue a
human session yet). To exercise the `RoleService` path end to end:

```sh
docker compose up -d enterprise-auth
TOKEN=$(docker compose run --rm enterprise-auth -mint-service-token=alerting)
# api: set ENTERPRISE_AUTH_URL=http://enterprise-auth:8082 and restart
# alerting: set API_SERVICE_TOKEN=$TOKEN and restart
```

```sh
docker build -f Dockerfile -t sentry-enterprise-auth .   # context is enterprise/, not the repo root
```

## Environment variables

| Var | Default |
|---|---|
| `HTTP_LISTEN_ADDR` | `:8082` |
| `POSTGRES_ADDR` | `localhost:5432` |
| `POSTGRES_DATABASE` | `sentry_metadata` |
| `POSTGRES_USERNAME` | `sentry` |
| `POSTGRES_PASSWORD` | (empty) |
| `OIDC_ISSUER_URL` | (empty — OIDC discovery skipped if unset) |
| `OIDC_CLIENT_ID` | (empty) |
| `OIDC_CLIENT_SECRET` | (empty) |
| `OIDC_REDIRECT_URL` | (empty) |
| `SAML_ENTITY_ID` | (empty) |
| `SAML_ACS_URL` | (empty) |
| `SAML_IDP_METADATA_URL` | (empty — presence only feeds `GET /auth/features`; not yet fetched/parsed) |
| `ENTERPRISE_SESSION_SIGNING_KEY` | **required**, min 32 bytes |
