# Phase 4 runbook

Extends `/docs/phase-0-runbook.md` through `/docs/phase-3-runbook.md`
with SSO plumbing, RBAC enforcement, tenant-scoped dashboards, audit
logging, and a Kubernetes deployment path. Read those first.

## Verification status — read this before the rest of this doc

Every prior phase's runbook documents claims **checked against the live
stack**, not asserted. This one is different, and says so plainly rather
than papering over it: **this session had no working Docker daemon
access and no reachable Kubernetes cluster**, so most of what follows is
a *procedure to run*, not a report of what was already run and passed.
Two exceptions, genuinely verified live against a real Postgres during
earlier Phase 4 tasks (see their own doc comments for the exact `docker
run` invocations):

- `enterprise/internal/audit`'s hash-chain, tamper-detection, and
  concurrent-write guarantees (task 4).
- `enterprise/internal/rbacstore`'s CRUD, run against a live Postgres
  the same way.

Everything else below — the auth-enforcement walkthrough, the dashboards
tenant-scoping fix, the Helm chart, the tenant-operator — has unit/fake-
client/`helm template` coverage (all passing, see each component's own
`go test`/`helm lint` output) but has **not** been exercised against a
real running stack in this session. If you're reading this to decide
whether Phase 4 is production-ready: it isn't yet, independent of this
gap — see `/docs/security/threat-model.md`'s headline finding (log-data
query isolation isn't built). This runbook exists so the first person
with real Docker/K8s access can actually close the loop, not to claim
that already happened.

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
**not** prove either tenant has a working ClickHouse database, since
`enterprise/internal/tenantprovision` (the piece that would create one)
isn't built. See `/deploy/README.md` and
`/docs/security/threat-model.md`.

## Known gaps (do not treat this phase as done without reading these)

Full accounting: `/docs/security/threat-model.md`. Headline items:

- **No tenant isolation on log data.** `POST /query` executes against
  one shared ClickHouse connection and one shared Tantivy index for
  every tenant, regardless of RBAC. This is Phase 4's originally-stated
  highest-risk item and it is not resolved.
- **No human SSO login.** OIDC/SAML protocol wiring exists;
  the HTTP login/callback handlers that would use it don't.
- **No per-resource dashboard grants** (`dashboard_permissions` has a
  schema, no handler reads it).
- Four adversarial ClickHouse/Tantivy probes named in
  `/docs/phase-4-isolation-design.md`'s verification plan are stubbed as
  explicitly-skipped tests in `api/internal/queryapi/
  tenant_isolation_gap_test.go`, blocked on the tenant-scoped connection
  work above.

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
(`api/internal/authz.RequireRole`'s doc comment). If this regresses, it
breaks every existing Phase 0-3 deployment silently.

**A dashboard created by one tenant is visible to another.**
This is the exact bug found and fixed in task 7 — see
`/docs/security/threat-model.md`'s "application-layer tenant scoping"
section and `api/internal/dashboards/handler_test.go`'s
`TestCrossTenant*` tests. If this regresses, `Handler.tenantID` or
`store.go`'s `WHERE tenant_id = ...` filters have been bypassed
somewhere — check every store method still takes and uses a `tenantID`
parameter.

**`helm template` fails with `error calling include: ... can't evaluate
field Release in type string`.**
A call site is passing a bare string to `sentry.selectorLabels` instead
of `(list $ "name")` — see `templates/_helpers.tpl`'s doc comment for
why the plain-string form doesn't work with `include`.
