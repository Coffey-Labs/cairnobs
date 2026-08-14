# enterprise

**Commercial license, not AGPLv3** — see `/CLAUDE.md`'s licensing
boundary. SSO (OIDC/SAML), tenant provisioning, and RBAC. Nothing in
`/agent`, `/ingest`, `/storage`, `/api`, `/web` core, or `/cli` imports
from this module — confirmed by `hack/check-tenant-boundary.sh`, run in
CI. `enterprise/` supplies tenant-scoped implementations of core's
already-shipped `api/querylang/executor.SQLRunner`/
`SearchClient` interfaces rather than core growing tenant awareness —
see `/docs/phase-4-isolation-design.md` for why.

## Status

What's built and wired end-to-end. Verification status varies by
piece -- `internal/audit` was confirmed against a real Postgres earlier
in this phase's work; everything else below has real integration tests
written the same way (skipped unless a live database's connection
details are supplied via env var, same pattern throughout this package)
but they have **not actually been run against a live database in this
environment** -- see `/docs/phase-4-runbook.md`'s verification-status
section for exactly what "not yet run" means here and why. Don't read
"has a test for this" as "this was confirmed to work."

- `internal/session` issues/validates signed (HS256/JWT) tokens for both
  human sessions and `/alerting`'s `RoleService` credential.
- `internal/authhandler` serves `POST /internal/authorize` (the endpoint
  `api/authz.HTTPAuthorizer` calls) and `GET /auth/features`
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
  `tenant_memberships`/`data_sources` (`metadata/migrations/0017-0032`).
- `internal/tenantprovision`: real `CREATE DATABASE`/`CREATE USER`/
  `GRANT` against ClickHouse. Its tests assert a tenant A user cannot
  read tenant B's database by fully-qualified name, and that
  `system.query_log`/`system.tables`/`SHOW DATABASES` don't leak across
  tenants either (task 2's finding was that the latter is
  version-dependent) -- not yet run against a live ClickHouse in this
  environment, see the note above.
- `internal/chrunner`: the tenant-scoped `SQLRunner` -- a per-tenant
  connection registry that resolves which tenant's ClickHouse connection
  to use from the authenticated identity in request context, never a
  parameter. Same adversarial probe, now through the actual production
  code path (`chrunner.Registry.RunSQL`, not just tenantprovision's raw
  grants).
- `internal/audit.QueryAPILogger`: the real `api/queryapi.AuditLogger`
  implementation -- wired into `enterprise-api`, no longer `nil`.
- `internal/loginhandler`: `GET /auth/oidc/login` + `GET /auth/oidc/callback`
  -- the actual human login flow, previously entirely missing. Redirects
  to the configured IdP with CSRF-protection state in a short-lived
  cookie, exchanges the code, verifies the ID token via `internal/oidc`,
  upserts a `users` row, resolves tenant/role from exactly one
  `tenant_memberships` row (refuses with a clear error on zero or
  multiple -- no tenant-picker UI yet), and issues a session cookie.
  **This one genuinely is verified**, unlike the ClickHouse pieces above:
  `loginhandler_test.go` runs the full flow against a real fake IdP
  (`coreos/go-oidc`'s own `oidctest` package, real RS256 signing and
  verification, no live database or Docker needed) and every test
  passes. Not yet tried against a real external IdP or a running
  `enterprise-auth` container.
- `cmd/enterprise-api`: a second binary (alongside `api/cmd/api`,
  unchanged) importing *both* `api`'s handler packages and the
  tenant-aware implementations above -- see its own doc comment for why
  this shape exists (`enterprise → api` is the allowed import direction;
  `api` can never import `enterprise/`). `-provision-tenant=<id>` is the
  operator action that provisions ClickHouse and marks a tenant active,
  same "offline action, not a network endpoint" shape as
  `enterprise-auth -mint-service-token`.

**Deliberately deferred, not half-built** -- named explicitly rather than
silently left out:
- SAML's login handler (the ACS endpoint) -- `internal/saml` does the
  protocol mechanics (AuthnRequest generation, assertion validation);
  nothing calls it from an HTTP handler, following `internal/
  loginhandler`'s now-built OIDC pattern once someone builds it.
- A tenant-picker UI/flow for an identity with more than one
  `tenant_memberships` row -- `loginhandler` refuses these logins
  outright rather than guessing (`ErrMultipleMemberships`).
- `dashboard_permissions` CRUD (schema exists,
  `metadata/migrations/0024`; no caller reads per-resource grants
  yet -- `dashboards`' handler enforces tenant-baseline role only, not
  the matrix's "(own/granted)" qualifier).
- `internal/searchclient` (the Tantivy-side sibling of `chrunner`) --
  `enterprise-api` shares the single, un-tenant-scoped Tantivy index
  every deployment does today (`api/searchclient.Dial`, unchanged). See
  `/docs/security/threat-model.md`.
- Any deployment-topology mechanism that actually routes traffic to
  `enterprise-api` instead of `api` -- both binaries exist,
  `docker-compose.yml` includes `enterprise-api` available but not
  wired into `web`'s default base URL, and the Helm chart has no
  service for it at all yet.

## Package layout

```
cmd/enterprise-auth/   config loading, OIDC discovery at startup, health/authorize/features endpoints, -mint-service-token
cmd/enterprise-api/     multi-tenant-aware alternative to api/cmd/api -- see its own doc comment
internal/tenant/        the ID type -- see its package doc comment before touching it
internal/oidc/           coreos/go-oidc wiring: discovery, login redirect, code exchange + ID token verification
internal/saml/            crewjam/saml wiring: SP setup, login redirect, response parsing/validation
internal/session/          issues/validates signed session + RoleService tokens
internal/authhandler/       POST /internal/authorize, GET /auth/features
internal/loginhandler/       GET /auth/oidc/login, GET /auth/oidc/callback -- the human login flow
internal/rbacstore/          users/tenants/tenant_memberships/data_sources CRUD (pgx against sentry_metadata)
internal/tenantprovision/     real ClickHouse CREATE DATABASE/USER/GRANT
internal/chrunner/             tenant-scoped api/querylang/executor.SQLRunner
internal/audit/            append-only, hash-chained query audit log, plus the
                            api/queryapi.AuditLogger adapter (queryapi_adapter.go)
internal/apiconfig/       enterprise-api's own env-var config
internal/config/          enterprise-auth's env-var config
```

Future additions: `internal/searchclient`, the OIDC/SAML login/callback
HTTP handlers, `dashboard_permissions` CRUD, and real deployment-topology
wiring for `enterprise-api` -- see "Status" above.

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

`internal/tenantprovision` and `internal/chrunner` need a real
ClickHouse instead (they mount the repo root, not just `enterprise/`,
since `internal/chrunner` imports `api/authz`/`api/querylang/executor`
via `go.mod`'s `replace` directives to `../api`):

```sh
docker run --rm --network sentry_default -v $(pwd)/..:/src -w /src/enterprise \
  -e TENANTPROVISION_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
  -e TENANTPROVISION_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/tenantprovision/... -v

docker run --rm --network sentry_default -v $(pwd)/..:/src -w /src/enterprise \
  -e CHRUNNER_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
  -e CHRUNNER_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/chrunner/... -v
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

## Provisioning a tenant and running `enterprise-api`

```sh
docker compose build enterprise-api   # context is the repo root, not enterprise/ -- see cmd/enterprise-api/Dockerfile
docker compose run --rm enterprise-api -provision-tenant=acme -display-name="Acme Corp"
docker compose up -d enterprise-api
curl -s http://localhost:8083/healthz
```

`-provision-tenant` creates the tenant/data_source rows in rbacstore if
they don't exist, provisions ClickHouse, persists the credentials, and
marks the tenant active -- refuses to run twice for the same tenant
(re-provisioning would either rotate a live credential or silently fail
to, see `tenantprovision.ProvisionClickHouse`'s doc comment). `web`
still points at plain `api` by default (`VITE_API_BASE_URL`) --
pointing it at `enterprise-api` instead is a manual `docker-compose.yml`
edit today, not a supported flag.

## Environment variables (`enterprise-auth`)

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
| `OIDC_REDIRECT_URL` | (empty — must be `<enterprise-auth base URL>/auth/oidc/callback`, registered with the IdP) |
| `SAML_ENTITY_ID` | (empty) |
| `SAML_ACS_URL` | (empty) |
| `SAML_IDP_METADATA_URL` | (empty — presence only feeds `GET /auth/features`; not yet fetched/parsed) |
| `ENTERPRISE_SESSION_SIGNING_KEY` | **required**, min 32 bytes |
| `POST_LOGIN_REDIRECT_URL` | `http://localhost:3000` — where the browser lands after `internal/loginhandler` sets a session cookie |

## Environment variables (`enterprise-api`)

| Var | Default |
|---|---|
| `HTTP_LISTEN_ADDR` | `:8083` |
| `CLICKHOUSE_ADDR` | `localhost:9000` |
| `CLICKHOUSE_ADMIN_USERNAME` | `default` |
| `CLICKHOUSE_ADMIN_PASSWORD` | (empty) |
| `SEARCH_GRPC_ADDR` | `localhost:50052` |
| `POSTGRES_ADDR` | `localhost:5432` |
| `POSTGRES_DATABASE` | `sentry_metadata` |
| `POSTGRES_USERNAME` | `sentry` |
| `POSTGRES_PASSWORD` | (empty) |
| `AUDIT_WRITER_USERNAME` | `audit_writer` |
| `AUDIT_WRITER_PASSWORD` | (empty) |
| `ENTERPRISE_AUTH_URL` | (empty — RBAC becomes a no-op, but `chrunner.Registry.RunSQL` still refuses every query with no resolved tenant identity, so leaving this unset does not mean "open access," it means "every query fails") |
| `CORS_ALLOWED_ORIGIN` | `*` |
| `QUERY_TIMEOUT_SECONDS` | `30` |
