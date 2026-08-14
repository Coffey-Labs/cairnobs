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
- `internal/searchclient`: the Tantivy-side sibling of `chrunner` --
  implements `api/querylang/executor.SearchClient`, resolving
  `SearchRequest.tenant_id` (new field, `proto/sentry/search/v1/
  search.proto`) from the authenticated request identity, same
  fail-closed shape as `chrunner.Registry.RunSQL`. Paired with
  `search/src/registry.rs`'s `IndexRegistry` (Rust, opens a per-tenant
  Tantivy index on demand). **Both genuinely verified** -- unlike the
  ClickHouse pieces, Tantivy is an embedded library, so the isolation
  probe (three tenants, shared search term, scoped search returns only
  that tenant's document) actually ran: `search`'s
  `cargo test`/`cargo clippy --all-targets -- -D warnings` and this
  package's `go test` both pass clean, no Docker or live database
  needed for either. `Client` also carries a `TenantChecker` (backed by
  `rbacstore.TenantIsActive`) since `search/src/registry.rs`'s
  `IndexRegistry` opens-or-creates an index for any syntactically-valid
  `tenant_id` -- a real gap found while closing
  `/docs/phase-4-isolation-design.md`'s verification-plan item 4: a
  mid-provisioning tenant would otherwise get a silently-empty search
  result instead of a refusal. Verified the same Docker-free way.
- `cmd/enterprise-api`: a second binary (alongside `api/cmd/api`,
  unchanged) importing *both* `api`'s handler packages and the
  tenant-aware implementations above -- see its own doc comment for why
  this shape exists (`enterprise → api` is the allowed import direction;
  `api` can never import `enterprise/`). `-provision-tenant=<id>` is the
  operator action that provisions ClickHouse and marks a tenant active,
  same "offline action, not a network endpoint" shape as
  `enterprise-auth -mint-service-token`.

OIDC and SAML login are both now fully wired: `internal/loginhandler`
serves `GET /auth/oidc/login`+`GET /auth/oidc/callback` and
`GET /auth/saml/login`+`POST /auth/saml/acs`, converging on the same
upsert-user/resolve-tenant/issue-session path. Both are verified the
same way -- a real fake IdP with genuine cryptographic signing and
verification (`coreos/go-oidc`'s `oidctest` for OIDC,
`crewjam/saml/samlidp` for SAML), no Docker needed, every test in
`loginhandler_test.go`/`saml_test.go` passing including the full login
round trip and negative paths (bad state/`InResponseTo`, expired/missing
credential, no/multiple tenant memberships). Writing the SAML test
caught two real bugs in `internal/saml.ParseResponse`, both fixed:
missing `r.ParseForm()` before reading the POSTed `SAMLResponse` field,
and email-attribute matching that missed the standard LDAP "mail" OID
(`urn:oid:0.9.2342.19200300.100.1.3`) that IdPs send by default absent
an explicit `AttributeConsumingService` request for "email" -- exactly
what `samlidp`'s own default assertion builder does. Neither protocol
has been tried against a real external IdP or a running
`enterprise-auth` container -- see `/docs/phase-4-runbook.md` §3a/§3b.

`dashboard_permissions` is now wired end to end: `rbacstore/
dashboard_permissions.go` is the raw CRUD, `rbacstore/
dashboards_adapter.go`'s `DashboardPermissions` implements
`api/dashboards.PermissionStore` (the interface core defines and
carries as a nil-by-default field, same shape as
`queryapi.AuditLogger`), and `enterprise-api`'s `main.go` wires it in.
`api/dashboards`' handler now enforces the matrix's "(own/granted)"
qualifier: an Editor may edit/delete a dashboard (or its panels) they
created, or one where a grant raises their effective role to Editor;
managing grants themselves is stricter still -- creator or Admin/Owner
only, closing a self-escalation path where a granted-but-not-creator
Editor could otherwise extend their own access. `metadata/migrations/
0033_restrict_dashboard_permissions_role.sql` fixes a divergence
between 0024's actual CHECK constraint (allowed `role='admin'`, nullable
`granted_by`) and the design doc's schema (viewer/editor only,
`granted_by` required) found while wiring this up. Verified against a
fake `PermissionStore` in `api/dashboards/handler_test.go` (the full
own/granted/admin/creator matrix, including the granted-editor-cannot-
manage-grants regression); real integration tests exist in
`rbacstore_test.go` but haven't run against a live Postgres in this
environment, same disclosed gap as the rest of this package's
Postgres-backed pieces.

**Deliberately deferred, not half-built** -- named explicitly rather than
silently left out:
- A tenant-picker UI/flow for an identity with more than one
  `tenant_memberships` row -- `loginhandler` refuses these logins
  outright rather than guessing (`ErrMultipleMemberships`).
- **Ingest tenant-awareness, for either storage engine** -- `chrunner`/
  `searchclient` prove read isolation given tenant-scoped data exists,
  but nothing writes it: every record `ingest` produces still lands in
  the single shared ClickHouse database and the single shared Tantivy
  index. A newly-provisioned tenant's storage is real and isolated, and
  permanently empty. Undesigned, not just unbuilt -- see
  `/docs/security/threat-model.md`.
- Any deployment-topology mechanism that actually routes traffic to
  `enterprise-api` instead of `api` -- both binaries exist,
  `docker-compose.yml` includes `enterprise-api` available but not
  wired into `web`'s default base URL, and the Helm chart has no
  service for it at all yet. **This is now the single largest gap** --
  both storage engines' isolation mechanisms themselves are built.

## Package layout

```
cmd/enterprise-auth/   config loading, OIDC discovery at startup, health/authorize/features endpoints, -mint-service-token, -create-tenant, -grant-membership-*
cmd/enterprise-api/     multi-tenant-aware alternative to api/cmd/api -- see its own doc comment
internal/tenant/        the ID type -- see its package doc comment before touching it
internal/oidc/           coreos/go-oidc wiring: discovery, login redirect, code exchange + ID token verification
internal/saml/            crewjam/saml wiring: SP setup, login redirect, response parsing/validation
internal/session/          issues/validates signed session + RoleService tokens
internal/authhandler/       POST /internal/authorize, GET /auth/features
internal/loginhandler/       GET /auth/oidc/{login,callback} + GET /auth/saml/login + POST /auth/saml/acs -- the human login flow
internal/rbacstore/          users/tenants/tenant_memberships/data_sources/dashboard_permissions CRUD (pgx against sentry_metadata)
internal/tenantprovision/     real ClickHouse CREATE DATABASE/USER/GRANT
internal/tenantcrd/            syncs -provision-tenant's real result into deploy/operator's Tenant CRD (K8s dynamic client, no cluster needed to test)
internal/chrunner/             tenant-scoped api/querylang/executor.SQLRunner
internal/searchclient/          tenant-scoped api/querylang/executor.SearchClient
internal/audit/            append-only, hash-chained query audit log, plus the
                            api/queryapi.AuditLogger adapter (queryapi_adapter.go)
internal/apiconfig/       enterprise-api's own env-var config
internal/config/          enterprise-auth's env-var config
```

Future additions: ingest tenant-awareness (undesigned), and real
deployment-topology wiring for `enterprise-api` -- see "Status" above.

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

Off by default (see "Status" above). To exercise the `RoleService` path
end to end:

```sh
docker compose up -d enterprise-auth
TOKEN=$(docker compose run --rm enterprise-auth -mint-service-token=alerting)
# api: set ENTERPRISE_AUTH_URL=http://enterprise-auth:8082 and restart
# alerting: set API_SERVICE_TOKEN=$TOKEN and restart
```

```sh
docker build -f Dockerfile -t sentry-enterprise-auth .   # context is enterprise/, not the repo root
```

## Bootstrapping a tenant and its first human user

`-create-tenant`/`-grant-membership-*` are `enterprise-auth` operator
flags, same "offline action gated by access to enterprise-auth's own
environment, not a network-reachable endpoint" shape as
`-mint-service-token` -- deliberately not an authenticated HTTP admin
API, which would have to solve "who's allowed to create the very first
tenant/membership" itself. Replaces what used to be a manual `psql`
dance (see `/docs/phase-4-runbook.md` §3a's history if you're wondering
why old references to it still show up in git blame):

```sh
docker compose run --rm enterprise-auth -create-tenant=acme -display-name="Acme Corp"
# Log in once via /auth/oidc/login or /auth/saml/login -- it fails with
# "no tenant membership" (403), but UpsertUserBySSO already created the
# users row by then, which -grant-membership-user-email needs.
docker compose run --rm enterprise-auth \
  -grant-membership-tenant=acme -grant-membership-user-email=you@example.com -grant-membership-role=owner
```

`-create-tenant` only touches `rbacstore` -- pair with `enterprise-api
-provision-tenant` (below) for a tenant to actually be able to run
queries, not just log in. `role=owner` also calls `SetOwner`, since a
tenant's Owner is a dedicated `tenants.owner_user_id` column, not just
the highest `tenant_memberships` role. Not yet built: revoking a
membership, listing a tenant's members, or a flag for
`dashboard_permissions` grants (those go through the HTTP endpoints
`api/dashboards`' handler now exposes -- `PUT`/`DELETE
/dashboards/{id}/permissions/{userId}`, `GET .../permissions`).

## Provisioning a tenant and running `enterprise-api`

`api`/`enterprise-api` are mutually exclusive in `docker-compose.yml`,
gated behind `COMPOSE_PROFILES` (`.env` checks in `single-tenant`, i.e.
plain `api`, as the zero-config default -- mirrors Helm's
`enterprise.enabled` flag). `-provision-tenant` itself doesn't bind a
port, so it runs fine regardless of the active profile; actually
serving traffic on `enterprise-api` needs the `enterprise` profile
active, since it now binds the same host port (8080) plain `api` does
(it gets a `default.aliases: [api]` network alias too, so `alerting`'s
`API_QUERY_URL`/`web`'s `VITE_API_BASE_URL` need zero changes either
way):

```sh
COMPOSE_PROFILES=enterprise docker compose build enterprise-api   # context is the repo root, not enterprise/ -- see cmd/enterprise-api/Dockerfile
COMPOSE_PROFILES=enterprise docker compose run --rm enterprise-api -provision-tenant=acme -display-name="Acme Corp"
COMPOSE_PROFILES=enterprise docker compose up -d enterprise-api
curl -s http://localhost:8080/healthz
```

`docker-compose.yml` has no Kubernetes cluster to sync into, so
`TENANT_CRD_NAMESPACE` is never set here -- `-provision-tenant`'s
`internal/tenantcrd` sync step is a documented no-op in this deployment
shape, same as everywhere else this codebase has an "off unless
configured" optional dependency. It only does anything in a real
cluster with `deploy/helm/sentry`'s `tenantOperator.enabled=true` -- see
`/deploy/helm/sentry/README.md`'s "Trying the two-tenant example."

`-provision-tenant` creates the tenant/data_source rows in rbacstore if
they don't exist, provisions ClickHouse, persists the credentials, and
marks the tenant active -- refuses to run twice for the same tenant
(re-provisioning would either rotate a live credential or silently fail
to, see `tenantprovision.ProvisionClickHouse`'s doc comment).

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
| `SAML_IDP_METADATA_URL` | (empty — SAML disabled if unset; if set, fetched and parsed at startup via `samlsp.FetchMetadata`, same trust level as `OIDC_ISSUER_URL`'s discovery fetch) |
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
