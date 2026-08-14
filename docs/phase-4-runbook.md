# Phase 4 runbook

Extends `/docs/phase-0-runbook.md` through `/docs/phase-3-runbook.md`
with SSO plumbing, RBAC enforcement, tenant-scoped dashboards, audit
logging, and a Kubernetes deployment path. Read those first.

## Verification status — read this before the rest of this doc

Every prior phase's runbook documents claims **checked against the live
stack**, not asserted. This one is different, and says so plainly rather
than papering over it: for the great majority of this phase's work,
**there was no working Docker daemon access and no reachable Kubernetes
cluster**, so most of what follows is a *procedure to run*, not a report
of what was already run and passed. Two genuine exceptions:

- `enterprise/internal/audit`'s hash-chain, tamper-detection, and
  concurrent-write guarantees (task 4) -- verified live against a real
  Postgres earlier in this phase's work (see its own doc comments for
  the exact `docker run` invocations), before the environment lost
  Docker access.
- `enterprise/internal/loginhandler`'s full OIDC login flow (§3a) --
  verified with real cryptography (a fake IdP that signs and verifies
  genuine RS256 tokens) *without* needing Docker or a live database at
  all, so this one was actually run in this runbook's own session, not
  just an earlier one. What's still unverified is wiring it into a real
  running `enterprise-auth` container against a real external IdP.

Everything else — `internal/rbacstore`'s CRUD, the auth-enforcement
walkthrough, the dashboards tenant-scoping fix, the Helm chart, the
tenant-operator, and (newest) `internal/tenantprovision`/
`internal/chrunner`'s live-ClickHouse tests — has unit/fake-client/
`helm template` coverage (all passing, see each component's own `go
test`/`helm lint` output, including every `Skip*`-gated integration test
confirmed to skip cleanly offline) but has **not** been exercised
against a real running stack. Be specific when citing this runbook: "the
tests exist and pass structurally" is a true, verified claim; "isolation
was confirmed against real ClickHouse" is not, yet. If you're reading
this to decide whether Phase 4 is production-ready: it isn't yet,
independent of this gap — see `/docs/security/threat-model.md`'s
headline finding. This runbook exists so the first person with real
Docker/K8s access can actually close the loop, not to claim that already
happened.

## 1. Bring up the stack

```sh
docker compose build enterprise-auth api alerting web
docker compose up -d
docker compose ps
```

New service beyond Phase 3: `enterprise-auth` (port 8082) — see
`enterprise/README.md`. Not wired into `api`/`alerting`'s enforcement by
default (`docker-compose.yml`'s comment on why: no OIDC/SAML login flow
exists yet, so turning on enforcement by default would break the web UI
and `sentryctl` with no way to log in).

## 2. Confirm Phase 0-3 behavior is unchanged

Every existing single-tenant flow must still work exactly as before —
this is the regression check for the nil-authorizer no-op design running
through every piece of Phase 4 auth wiring:

```sh
curl -s -X POST http://localhost:8080/query -H 'Content-Type: application/json' -d '{"query":"stats count"}'
sentryctl dashboards list
curl -s http://localhost:8081/healthz
```

All three should behave exactly as in the Phase 3 runbook — no auth
required, since `ENTERPRISE_AUTH_URL`/`API_SERVICE_TOKEN` aren't set.

## 3. `enterprise-auth`: mint and validate a service token

```sh
curl -s http://localhost:8082/healthz && echo " <- OK"

TOKEN=$(docker compose run --rm enterprise-auth -mint-service-token=alerting)
echo "$TOKEN"

curl -s -X POST http://localhost:8082/internal/authorize -H "Authorization: Bearer $TOKEN"
# expect: {"tenant_id":"","user_id":"","role":"service"}

curl -s -o /dev/null -w "invalid token -> %{http_code}\n" \
  -X POST http://localhost:8082/internal/authorize -H "Authorization: Bearer garbage"
# expect: 401

curl -s http://localhost:8082/auth/features
# expect: {"sso_configured":false,"oidc_enabled":false,"saml_enabled":false}
# (no OIDC_ISSUER_URL/SAML_IDP_METADATA_URL set in this compose file)
```

## 3a. `enterprise-auth`: human login via OIDC (new -- unlike everything
else in this runbook, the underlying flow *was* verified live in this
session, just not against a real running `enterprise-auth` container or
a real external IdP)

`enterprise/internal/loginhandler`'s tests already prove the mechanism
works end to end against a real fake IdP (`go test
./internal/loginhandler/... -v` from `enterprise/`, no Docker needed --
see `enterprise/README.md`). What's still unverified is wiring it into
this actual running stack. To try that for real, point
`docker-compose.yml`'s `enterprise-auth` service at a real OIDC IdP
(a free Auth0/Okta developer tenant, or any IdP you control):

```sh
# Add to enterprise-auth's environment in docker-compose.yml (or a
# docker-compose.override.yml):
#   OIDC_ISSUER_URL: "https://your-tenant.example.com/"
#   OIDC_CLIENT_ID: "..."
#   OIDC_CLIENT_SECRET: "..."
#   OIDC_REDIRECT_URL: "http://localhost:8082/auth/oidc/callback"
# Register that same redirect URL with the IdP's application config.

docker compose up -d --build enterprise-auth
curl -s http://localhost:8082/auth/features
# expect: {"sso_configured":true,"oidc_enabled":true,"saml_enabled":false}
```

Before a login can succeed, the logging-in identity needs a
`tenant_memberships` row -- there's no admin UI for this yet, so insert
one directly:

```sh
docker run --rm --network sentry_default postgres:16-alpine psql \
  "postgres://sentry:sentry-dev-only@metadata-postgres:5432/sentry_metadata" -c \
  "INSERT INTO tenants (id, display_name, status) VALUES ('acme', 'Acme Corp', 'active') ON CONFLICT DO NOTHING;"
# The users row is created automatically on first login (UpsertUserBySSO)
# -- but tenant_memberships needs the user's ID, which doesn't exist
# until after a first login attempt fails with 403. Log in once (it'll
# fail with "no tenant membership"), then:
docker run --rm --network sentry_default postgres:16-alpine psql \
  "postgres://sentry:sentry-dev-only@metadata-postgres:5432/sentry_metadata" -c \
  "SELECT id, email FROM users;"
docker run --rm --network sentry_default postgres:16-alpine psql \
  "postgres://sentry:sentry-dev-only@metadata-postgres:5432/sentry_metadata" -c \
  "INSERT INTO tenant_memberships (id, tenant_id, user_id, role) VALUES (gen_random_uuid(), 'acme', '<user id from above>', 'viewer');"
```

Then visit `http://localhost:8082/auth/oidc/login` in a real browser,
complete the IdP's login, and confirm you land on
`POST_LOGIN_REDIRECT_URL` (`http://localhost:3000` by default) with a
`sentry_session` cookie set. This whole bootstrap sequence (manual SQL
to create the first tenant membership) is exactly the kind of rough
edge an admin UI would smooth over -- named as real future work, not
hidden.

## 4. Turn on RBAC enforcement and prove it actually blocks/allows

Without touching the main stack's `api` container (so step 2's baseline
keeps working):

```sh
docker compose run --rm -d --name sentry-api-enforced -p 8090:8080 \
  -e ENTERPRISE_AUTH_URL=http://enterprise-auth:8082 api

curl -s -o /dev/null -w "no auth -> %{http_code} (want 401)\n" \
  -X POST http://localhost:8090/query -H 'Content-Type: application/json' -d '{"query":"stats count"}'
curl -s -o /dev/null -w "with service token -> %{http_code} (want 200)\n" \
  -X POST http://localhost:8090/query -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"query":"stats count"}'

docker stop sentry-api-enforced
```

`GET /dashboards` on the same enforced instance should return 401
without a token — there's no way to mint a human (Viewer/Editor/etc.)
session yet (no OIDC/SAML login handler exists — see
`enterprise/cmd/enterprise-auth/main.go`'s doc comment), so this
runbook can't walk through a real human RBAC scenario end to end. That
gap is real, not an oversight in this runbook.

## 5. Dashboards tenant scoping

This is the fix from Phase 4 task 7/8 (see `/docs/security/threat-model.md`)
— every dashboards query is now scoped to the authenticated identity's
tenant. Verify the real SQL, not just the fake-store unit tests:

```sh
docker run --rm --network sentry_default -v $(pwd)/api:/src -w /src \
  -e DASHBOARDS_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e DASHBOARDS_TEST_POSTGRES_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/dashboards/... -run Integration -v
```

Expect all `TestIntegration*` tests to pass, including
`TestIntegrationDashboardTenantForeignKeyRejectsUnknownTenant` (the
`tenant_id` foreign key added in
`metadata/migrations/0027_add_dashboards_tenant_fk.sql` rejecting a
dashboard for a tenant that doesn't exist).

## 6. `enterprise/internal/rbacstore` and `internal/audit` (already verified — reconfirm here)

```sh
docker run --rm --network sentry_default -v $(pwd)/enterprise:/src -w /src \
  -e RBACSTORE_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e RBACSTORE_TEST_POSTGRES_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/rbacstore/... -v

docker run --rm --network sentry_default -v $(pwd)/enterprise:/src -w /src \
  -e AUDIT_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e AUDIT_TEST_POSTGRES_PASSWORD=audit-writer-dev-only \
  -e AUDIT_TEST_ADMIN_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/audit/... -v
```

## 7. `deploy`: Helm chart and Operator (offline-only so far — see `/deploy/README.md`)

No live cluster was available to `kubectl apply` any of this. What can
be checked without one:

```sh
cd deploy/operator && go build ./... && go vet ./... && go test ./...

cd ../helm/sentry
helm lint .
helm template sentry . --include-crds > /tmp/default.yaml
helm template sentry . --include-crds \
  --set enterprise.enabled=true --set tenantOperator.enabled=true \
  --set 'tenants[0].name=acme' --set 'tenants[0].displayName=Acme Corp' \
  --set 'tenants[1].name=globex' --set 'tenants[1].displayName=Globex Corporation' \
  > /tmp/multitenant.yaml
```

With a real cluster reachable (`kind create cluster`, or similar):

```sh
docker build -f deploy/operator/Dockerfile -t sentry-tenant-operator deploy/operator/
kind load docker-image sentry-tenant-operator   # or push to a registry the cluster can pull from
helm install sentry deploy/helm/sentry --include-crds \
  --set tenantOperator.enabled=true --set enterprise.enabled=true \
  --set 'tenants[0].name=acme' --set 'tenants[0].displayName=Acme Corp'
kubectl get tenants
kubectl get secret sentry-tenant-acme-clickhouse -o yaml
```

Expect `kubectl get tenants` to show `acme` reach `status.phase: Active`
and the Secret to contain a generated `username`/`password`/`database`.
This proves the K8s-side half of a real two-tenant deployment — it does
**not** provision a working ClickHouse database itself (the Operator
manages the K8s Secret only); §8 below is the piece that actually
provisions ClickHouse.

## 8. `enterprise-api`: real per-tenant ClickHouse isolation

`enterprise/internal/tenantprovision` and `enterprise/internal/chrunner`
close the ClickHouse half of the headline gap §"Known gaps" below used
to describe as completely unbuilt. It's still a second binary you have
to choose to run, though — see `/docs/security/threat-model.md`'s "Read
this first" section. With OIDC login now built (§3a), a real
`curl -X POST http://localhost:8083/query` walkthrough as a logged-in
tenant is *possible* now, but still needs the manual `tenant_memberships`
bootstrap from §3a — a full end-to-end curl walkthrough isn't included
here yet.

```sh
docker compose build enterprise-api
docker compose run --rm enterprise-api -provision-tenant=acme -display-name="Acme Corp"
docker compose run --rm enterprise-api -provision-tenant=globex -display-name="Globex Corporation"
docker compose up -d enterprise-api
curl -s http://localhost:8083/healthz
```

Confirm isolation end to end against the live stack (this is the same
assertion `enterprise/internal/chrunner/chrunner_test.go`'s
`TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL` makes, run here
as an integration test instead of a curl walkthrough since a full login
walkthrough isn't scripted yet):

```sh
docker run --rm --network sentry_default -v $(pwd)/enterprise:/src -w /src \
  -e CHRUNNER_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
  -e CHRUNNER_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/chrunner/... -v

docker run --rm --network sentry_default -v $(pwd)/enterprise:/src -w /src \
  -e TENANTPROVISION_TEST_CLICKHOUSE_ADDR=clickhouse:9000 \
  -e TENANTPROVISION_TEST_CLICKHOUSE_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/tenantprovision/... -v
```

Expect all tests to pass, including
`TestProvisionedUserCannotReadSystemTables` (item 2 of
`/docs/phase-4-isolation-design.md`'s verification plan, closed this
pass) and `TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL` (item 1,
closed through the actual production code path, not just
tenantprovision's raw grants).

## 9. Tantivy per-tenant isolation — no Docker needed, actually run this one

Unlike everything above, this one doesn't need a live stack at all —
Tantivy is an embedded library, not a networked service, so both halves
(the Rust index registry and the Go client that talks to it) can be
verified with nothing but a local toolchain:

```sh
cd search
cargo build
cargo clippy --all-targets -- -D warnings
cargo test
# expect 14 tests passing, including
# registry::tests::tenant_index_is_isolated_from_default_and_other_tenants
# -- item 3 of /docs/phase-4-isolation-design.md's verification plan.

cd ../enterprise
go test ./internal/searchclient/... -v
# real in-process gRPC server, confirms SearchRequest.tenant_id is set
# correctly and that a request with no/invalid tenant identity is refused.
```

This is the one piece of Phase 4's tenant isolation work that has
**actually been run and confirmed passing** in an environment without
Docker access, alongside `enterprise/internal/loginhandler`'s OIDC
tests (§3a) — both are unusually strong evidence precisely because they
needed no infrastructure this environment lacked.

## Known gaps (do not treat this phase as done without reading these)

Full accounting: `/docs/security/threat-model.md`. Headline items:

- **Both storage engines' isolation exists but is opt-in.**
  `enterprise-api` (§8, §9) gives real per-tenant ClickHouse *and*
  Tantivy isolation, but plain `api` (still the default in
  `docker-compose.yml`/`web`'s base URL) has neither, and nothing flags
  which one a given deployment is actually running. This is now the
  single largest gap — not a missing mechanism, a missing enforcement/
  default.
- **Ingest has no tenant concept for either storage engine.** Every
  record `ingest` produces lands in the one shared ClickHouse database
  and the one shared Tantivy index no matter what. A newly-provisioned
  tenant's storage is real, isolated at query time, and permanently
  empty until this changes — undesigned, not just unbuilt.
- **Human SSO login now works for OIDC** (§3a) -- verified with a real
  fake IdP, not yet a real external one or a running `enterprise-auth`
  container. **SAML login still doesn't exist** -- protocol wiring only,
  no ACS handler. No tenant-picker UI for a multi-membership identity
  either (refused outright).
- No admin UI to create a `tenant_memberships` row -- §3a's manual SQL
  bootstrap is the only way to grant a logged-in identity access today.
- **No per-resource dashboard grants** (`dashboard_permissions` has a
  schema, no handler reads it).
- Three of the four adversarial ClickHouse/Tantivy probes named in
  `/docs/phase-4-isolation-design.md`'s verification plan are closed
  (§8, §9); the last (mid-provisioning-race handling) is still stubbed
  as an explicitly-skipped test in
  `api/queryapi/tenant_isolation_gap_test.go`.

## Tearing down

```sh
docker compose down -v
helm uninstall sentry   # if installed against a real cluster
```

## Troubleshooting

**`enterprise-auth` fails to start with "ENTERPRISE_SESSION_SIGNING_KEY
must be set to at least 32 bytes".**
Required, unlike OIDC/SAML config — see `enterprise/internal/config.Load`.
`docker-compose.yml`'s dev value is long enough; a custom override must
be too.

**`POST /query` returns 401 even though `ENTERPRISE_AUTH_URL` isn't
set.**
Check `api/cmd/api/main.go` actually left `authorizer` nil when
`cfg.EnterpriseAuthURL == ""` — a nil `Authorizer` must be a no-op
(`api/authz.RequireRole`'s doc comment). If this regresses, it
breaks every existing Phase 0-3 deployment silently.

**A dashboard created by one tenant is visible to another.**
This is the exact bug found and fixed in task 7 — see
`/docs/security/threat-model.md`'s "application-layer tenant scoping"
section and `api/dashboards/handler_test.go`'s
`TestCrossTenant*` tests. If this regresses, `Handler.tenantID` or
`store.go`'s `WHERE tenant_id = ...` filters have been bypassed
somewhere — check every store method still takes and uses a `tenantID`
parameter.

**`helm template` fails with `error calling include: ... can't evaluate
field Release in type string`.**
A call site is passing a bare string to `sentry.selectorLabels` instead
of `(list $ "name")` — see `templates/_helpers.tpl`'s doc comment for
why the plain-string form doesn't work with `include`.
