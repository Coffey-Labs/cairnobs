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
  upserts a `users` row, resolves tenant/role from `tenant_memberships`
  (refuses with a clear error on zero rows; more than one starts a real
  tenant-selection round trip -- `GET /auth/memberships` +
  `POST /auth/select-tenant`, backed by a short-lived
  `session.Manager.IssuePendingLogin` token -- rather than guessing; see
  "Tenant selection" below), and issues a session cookie.
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

## Tenant selection (multi-membership identities)

Both the backend protocol for choosing a tenant and the frontend page
that calls it (`web/src/routes/select-tenant`) are now built. When
`resolveIdentity` finds more than one `tenant_memberships` row for a
logged-in identity, `finishLogin`
issues a `session.Manager.IssuePendingLogin` token (a distinct Go/JWT
type from a real session -- see that type's doc comment for a real bug
this design caught in its own tests: a shared JSON key would have let a
full session token double as a pending login) as a
`sentry_pending_login` cookie (`Path=/auth`) and redirects to
`SELECT_TENANT_REDIRECT_URL` (defaults to
`{POST_LOGIN_REDIRECT_URL}/select-tenant`) instead of completing the
login. From there:

- `GET /auth/memberships` -- lists the pending identity's tenants
  (`tenant_id`, `tenant_display_name`, `role`) to choose between.
- `POST /auth/select-tenant` with `{"tenant_id": "..."}` -- re-derives
  the role for that specific tenant server-side (never trusts a
  client-supplied role, only checks the claimed `tenant_id` against the
  identity's actual memberships, refusing with 403 otherwise), issues
  the real session cookie, and responds with
  `{"redirect_url": "..."}` for the caller to navigate to -- JSON, not a
  redirect, since this is a POST/fetch call a frontend page should
  control the navigation for itself.

Verified with the same real-fake-IdP tests as the rest of this package
(`loginhandler_test.go`/`saml_test.go`): the full login -> pending
cookie -> `GET /auth/memberships` -> `POST /auth/select-tenant` -> real
session round trip, plus negative paths (missing/expired/wrong-type
pending cookie, a `tenant_id` outside the identity's actual
memberships, a real session token rejected when presented as a pending
login).

The frontend side needed two things `web` didn't have: session/cookie-
aware requests (`$lib/api.ts`'s `listMemberships`/`selectTenant`, both
`fetch(..., {credentials: 'include'})`) and CORS that actually allows a
credentialed cross-origin request -- `httpserver.WithCredentialedCORS`
(new, in `api/httpserver`, next to the plain `WithCORS` every other
service in this repo uses), wired in by this binary's `main.go` and
configured via the new `CORSAllowedOrigin` field
(`CORS_ALLOWED_ORIGIN`, defaulting to `PostLoginRedirectURL` --
`web`'s own origin is exactly what needs credentialed access here).
Browsers categorically refuse to honor `Access-Control-Allow-Origin: "*"`
on a credentialed request, which is why this couldn't reuse plain
`WithCORS`'s wildcard-friendly default the way `enterprise-api` does.
Genuinely verified in a real browser in this environment (see
`/web/README.md`'s "Tenant picker" section for exactly how, since no
live Postgres/IdP was needed to exercise `web`'s own fetch/CORS/cookie
wiring): the full cross-origin cookie round trip, a real click choosing
a tenant, and the post-selection redirect, plus the missing/expired
pending-login error path.

Two other things once named as deferred here are built too: ingest
write-routing, both ClickHouse (see "Ingest write-routing (ClickHouse)"
below) and Tantivy (`/search/README.md`'s "Per-tenant indices" section
-- needed no code in this module at all, since `search`'s
`IndexRegistry` already lived in AGPL core); and deployment-topology
routing (does traffic actually reach `enterprise-api` instead of
`api`), now a single-flag choice in both `deploy/helm/sentry` and
`docker-compose.yml` (`enterprise.enabled` / `COMPOSE_PROFILES`), see
CLAUDE.md.

## Ingest tenant identity

`ingest` (AGPL core) gained an optional `TenantResolver`
(`ingest/internal/grpcserver`) -- nil by default, the same "off unless
configured" shape as every other optional integration point in this
codebase. When `ENTERPRISE_AUTH_URL` is set, `PushBatch` requires an
`authorization: Bearer <token>` gRPC metadata entry on every call,
resolves it via a new `POST /internal/authorize-ingest` endpoint on
*this* service (`internal/authhandler`, backed by a new
`ingest_credentials` table in `internal/rbacstore` -- only a SHA-256
hash of the token is ever stored), and attaches the resolved tenant ID
to every record as a `tenant_id` Kafka message header before producing
it. Fail-closed: once a resolver is configured, a missing or invalid
credential refuses the whole batch, never falls back to "no tenant."

Mint a credential with `-create-ingest-credential-tenant=<id>` (prints
the plaintext token exactly once -- see `ingest_credentials`' migration
comment for why it can't be recovered again, only reissued);
`-list-ingest-credentials-tenant=<id>`/`-revoke-ingest-credential=<id>`
manage existing ones. `ingest`'s own HTTP client
(`ingest/internal/tenantresolver.HTTPResolver`) is the piece that
actually calls `/internal/authorize-ingest` -- never an `enterprise/`
import (`ingest` is AGPL core), same "network boundary, not import
boundary" shape `api/authz.HTTPAuthorizer` already uses for the query
path.

## Ingest write-routing (ClickHouse)

`enterprise/cmd/enterprise-ingest` is `ingest -mode=consumer`'s multi-
tenant alternative -- same "second binary" shape as `enterprise-api`
next to `api/cmd/api` (AGPL core must never import `enterprise/`, so the
tenant-aware wiring has to live in a binary that imports *into* core,
not the reverse). It reuses `ingest/consumer.Consumer`'s exact flush
loop unchanged, swapping in `enterprise/internal/chwriter.Registry` --
chrunner's write-side counterpart -- as the writer: one fully separate
`*ingest/clickhousewriter.Writer` (and the `driver.Conn` under it) per
tenant, built once at startup from `rbacstore.
ListProvisionedDataSources` (the same source of truth `chrunner` already
uses for reads). `WriteBatch` groups a Kafka batch's records by their
`tenant_id` tag and writes each tenant's group through its own
dedicated connection, refusing the whole call (matching `ingest/
consumer`'s existing all-or-nothing batch contract -- no offsets commit,
the batch redelivers) if any record is untagged or tagged with a tenant
that isn't provisioned. `ingest/consumer` and `ingest/clickhousewriter`
moved out of `internal/` for this -- same Go compiler-enforced
visibility reasoning as every other package this phase moved out of
`internal/` for a cross-module import (see `ingest/README.md`'s "Multi-
tenant write-routing" section).

A real bug was found and fixed while wiring this up:
`tenantprovision.ProvisionClickHouse` originally granted a tenant's
ClickHouse user `SELECT` only -- correct for `chrunner`'s query path,
but it would have made every real per-tenant write from `chwriter` fail
with a permission error, since it's the *same* credential used for
both. Fixed by granting `SELECT, INSERT` (not a second, separate
write-only credential -- there's no cross-tenant boundary crossed by
also granting INSERT within a tenant's own database, so one credential
for both directions is the simpler, still-correctly-scoped choice).

A real multi-tenant deployment runs `ingest -mode=server` (agent-facing,
tags records, unchanged) alongside `enterprise-ingest` (consumer,
per-tenant writes) *instead of* `ingest -mode=consumer` -- see `deploy/
helm/sentry`'s `ingest.requireTenantCredential` value (gates both the
credential-validation requirement and this mode split together, since
write-routing is only meaningful once records actually carry a
tenant_id to route on) and `docker-compose.yml`'s `enterprise-ingest`
service (a simpler opt-in there -- true `-mode=server`/`-mode=consumer`
exclusivity isn't wired in compose, a disclosed local-dev-only gap; see
that service's own comment).

Verified: `enterprise/internal/chwriter`'s fail-closed paths (empty/
unknown `tenant_id`) run genuinely without Docker (constructing a
`Registry` directly, bypassing `New`, which is the only part that would
dial ClickHouse); the actual per-tenant write-isolation probe
(`TestRegistryWritesEachTenantToItsOwnDatabase`) and the
`tenantprovision` INSERT-grant regression test are real integration
tests against a live ClickHouse, same `CHWRITER_TEST_CLICKHOUSE_ADDR`/
`TENANTPROVISION_TEST_CLICKHOUSE_ADDR` convention as every other
ClickHouse-backed test this phase -- not run against a live database in
this environment.

## Package layout

```
cmd/enterprise-auth/   config loading, OIDC discovery at startup, health/authorize/features/authorize-ingest endpoints, -mint-service-token, -create-tenant, -grant-membership-*, -revoke-membership-*, -list-memberships-tenant, -transfer-owner-*, -create-ingest-credential-tenant, -list-ingest-credentials-tenant, -revoke-ingest-credential
cmd/enterprise-api/     multi-tenant-aware alternative to api/cmd/api -- see its own doc comment
cmd/enterprise-ingest/   multi-tenant-aware alternative to ingest -mode=consumer -- see its own doc comment
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
internal/chwriter/              tenant-scoped ingest/consumer.chWriter -- chrunner's write-side counterpart
internal/searchclient/          tenant-scoped api/querylang/executor.SearchClient
internal/audit/            append-only, hash-chained query audit log, plus the
                            api/queryapi.AuditLogger adapter (queryapi_adapter.go)
internal/apiconfig/       enterprise-api's own env-var config
internal/ingestconfig/     enterprise-ingest's own env-var config
internal/config/          enterprise-auth's env-var config
```

`ingest/internal/tenantresolver` (AGPL core, not enterprise/, since
ingest must never import enterprise/) is the client side of `internal/
authhandler`'s new `POST /internal/authorize-ingest` -- see "Ingest
tenant identity" above.

Per-tenant write-routing for ingest is built for both ClickHouse (this
module's `cmd/enterprise-ingest` + `internal/chwriter`, see "Ingest
write-routing (ClickHouse)" above) and Tantivy (`search/src/consumer.rs`
+ `search/src/registry.rs`, entirely in AGPL core -- see
`/search/README.md`'s "Per-tenant indices" section, since Tantivy's lack
of a grant system meant there was never an import-boundary reason to put
any of it here).

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

# See who's actually in a tenant, and take access away again:
docker compose run --rm enterprise-auth -list-memberships-tenant=acme
docker compose run --rm enterprise-auth \
  -revoke-membership-tenant=acme -revoke-membership-user-email=someone-else@example.com

# Hand ownership to someone else -- the previous owner is downgraded to
# admin, not removed, so this doesn't need a -revoke-membership-* first:
docker compose run --rm enterprise-auth \
  -transfer-owner-tenant=acme -transfer-owner-user-email=new-owner@example.com
```

`-create-tenant` only touches `rbacstore` -- pair with `enterprise-api
-provision-tenant` (below) for a tenant to actually be able to run
queries, not just log in. `role=owner` also calls `SetOwner`, since a
tenant's Owner is a dedicated `tenants.owner_user_id` column, not just
the highest `tenant_memberships` role -- but only for a tenant's *first*
owner assignment (it refuses if a *different* owner already exists, per
`rbacstore.TransferOwner`'s doc comment). `-revoke-membership-*` refuses
to revoke a tenant's current Owner for the same reason `tenants.
owner_user_id` can only ever name one user --
`-transfer-owner-tenant`/`-transfer-owner-user-email` is the real
handoff: `rbacstore.TransferOwner` atomically downgrades the current
owner to `admin`, promotes the new owner, and updates
`tenants.owner_user_id`, all in one transaction (the first this package
uses -- every other mutation here is a single independent statement,
but leaving `owner_user_id` and `tenant_memberships` disagreeing mid-
operation is exactly the inconsistent state `RevokeMembership`'s doc
comment already worries about). Changing a non-Owner role is just
re-running `-grant-membership-*` with a different
`-grant-membership-role` (`SetMembership`'s upsert already supports
it). `dashboard_permissions` grants have no `enterprise-auth` flag and
don't need one -- `sentryctl dashboards permissions list|grant|revoke`
covers them over the HTTP endpoints `api/dashboards`' handler already
exposes (`PUT`/`DELETE /dashboards/{id}/permissions/{userId}`,
`GET .../permissions`), see `/cli/README.md`.

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
| `SELECT_TENANT_REDIRECT_URL` | `{POST_LOGIN_REDIRECT_URL}/select-tenant` — where the browser lands for a multi-membership identity instead; `web/src/routes/select-tenant` serves it, see "Tenant selection" above |
| `CORS_ALLOWED_ORIGIN` | `{POST_LOGIN_REDIRECT_URL}` — must be a literal origin, not `*` (unlike `enterprise-api`'s var of the same name below): `GET /auth/memberships`/`POST /auth/select-tenant` are credentialed requests, and browsers refuse to honor a wildcard `Access-Control-Allow-Origin` on those |

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
