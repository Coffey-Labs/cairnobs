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
- `enterprise/internal/loginhandler`'s full SAML login flow (§3b) --
  same bar as OIDC above, verified against a real fake SAML IdP
  (`crewjam/saml/samlidp`: genuine XML signing and signature
  verification, a real `AuthnRequest`/`Response` round trip), no Docker
  needed. Writing this test caught two real bugs in
  `enterprise/internal/saml`, now fixed: `ParseResponse` never called
  `r.ParseForm()` before reading the POSTed `SAMLResponse` field (every
  real ACS POST would have silently decoded to nothing), and the email-
  attribute matching didn't recognize `urn:oid:0.9.2342.19200300.100.1.3`
  (the standard LDAP "mail" OID), which is what an IdP sends by default
  when the SP doesn't explicitly request an attribute literally named
  "email" -- crewjam's own fake IdP hit this path. Same remaining gap as
  OIDC: not yet tried against a real external IdP or a running
  `enterprise-auth` container.

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
`tenant_memberships` row. There's still no admin UI for this, but as of
this runbook revision there's no manual SQL either --
`enterprise-auth`'s `-create-tenant`/`-grant-membership-*` operator
flags replace the old psql dance:

```sh
docker compose run --rm enterprise-auth -create-tenant=acme -display-name="Acme Corp"
# Log in once at http://localhost:8082/auth/oidc/login -- it'll fail
# with "no tenant membership" (403), but UpsertUserBySSO already created
# the users row by that point, which -grant-membership-user-email needs.
docker compose run --rm enterprise-auth \
  -grant-membership-tenant=acme -grant-membership-user-email=<the email you logged in with> -grant-membership-role=viewer
```

Then visit `http://localhost:8082/auth/oidc/login` again in a real
browser, complete the IdP's login, and confirm you land on
`POST_LOGIN_REDIRECT_URL` (`http://localhost:3000` by default) with a
`sentry_session` cookie set. `-create-tenant` only touches `rbacstore`
(control-plane/RBAC) -- it's independent of `enterprise-api
-provision-tenant`'s ClickHouse/Tantivy data-plane provisioning (§8),
so a tenant created this way can log users in immediately but can't yet
serve their queries until that's run too, the same "two separate
operator actions" gap named in "Known gaps" below. Not yet built: an
equivalent for revoking/listing memberships, or anything for
`dashboard_permissions` grants (§5a) beyond calling the HTTP endpoints
directly.

## 3b. `enterprise-auth`: human login via SAML (new -- same "verified
live in this session, not against a real running container or a real
external IdP" caveat as §3a)

`enterprise/internal/loginhandler`'s SAML tests already prove the
mechanism works end to end against a real fake SAML IdP (`go test
./internal/loginhandler/... -run SAML -v` from `enterprise/`, no Docker
needed). What's still unverified is wiring it into this actual running
stack. To try that for real, point `docker-compose.yml`'s
`enterprise-auth` service at a real SAML IdP (many identity providers
offer a free developer/trial tenant with SAML app support):

```sh
# Add to enterprise-auth's environment in docker-compose.yml (or a
# docker-compose.override.yml):
#   SAML_ENTITY_ID: "http://localhost:8082/saml/metadata"
#   SAML_ACS_URL: "http://localhost:8082/auth/saml/acs"
#   SAML_IDP_METADATA_URL: "https://your-idp.example.com/metadata"
# Register SAML_ENTITY_ID/SAML_ACS_URL with the IdP's application config
# -- the IdP needs Sentry's ACS URL to know where to POST the assertion.

docker compose up -d --build enterprise-auth
curl -s http://localhost:8082/auth/features
# expect: {"sso_configured":true,"oidc_enabled":false,"saml_enabled":true}
```

Bootstrapping the first `tenant_memberships` row uses the same
`-create-tenant`/`-grant-membership-*` flags as §3a (log in once, it
fails with 403, grant the membership using the email you logged in
with, log in again). Then visit
`http://localhost:8082/auth/saml/login` in a real browser, complete the
IdP's login, and confirm a `sentry_session` cookie lands after redirect
to `POST_LOGIN_REDIRECT_URL`. Note SAML's `sentry_saml_request` cookie
is `SameSite=None`, which requires `Secure` -- i.e. this only works over
HTTPS in a real deployment, unlike OIDC's redirect-based callback which
tolerates plain HTTP for local dev (see
`enterprise/internal/loginhandler/loginhandler.go`'s `handleSAMLLogin`
doc comment for why).

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
without a token — this section only demonstrates the service-token path
(§3/§3a/§3b cover minting a real human session via OIDC or SAML); walking
that session cookie through this same enforced instance to get a 200 is
left as the natural next verification step once real Docker/K8s access
exists, not yet done in this runbook.

## 5. Dashboards tenant scoping

This is the fix from Phase 4 task 7/8 (see `/docs/security/threat-model.md`)
— every dashboards query is now scoped to the authenticated identity's
tenant. Verify the real SQL, not just the fake-store unit tests:

```sh
docker run --rm --network sentry_default -v $(pwd)/api:/src -w /src \
  -e DASHBOARDS_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e DASHBOARDS_TEST_POSTGRES_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./dashboards/... -run Integration -v
```

(Path fixed from an earlier `./internal/dashboards/...` -- stale since
`dashboards` moved from `api/internal/dashboards` to `api/dashboards`
earlier in Phase 4, once `enterprise/cmd/enterprise-api` needed to
import it: Go's compiler-enforced `internal/` visibility rule meant a
separate module like `enterprise/` could never import anything under
`api/internal/...`, regardless of the AGPL/commercial licensing
boundary, which only forbids the reverse direction.)

Expect all `TestIntegration*` tests to pass, including
`TestIntegrationDashboardTenantForeignKeyRejectsUnknownTenant` (the
`tenant_id` foreign key added in
`metadata/migrations/0027_add_dashboards_tenant_fk.sql` rejecting a
dashboard for a tenant that doesn't exist).

## 5a. Per-resource dashboard grants (new -- built and unit-tested, not yet run live)

`enterprise/internal/rbacstore`'s `dashboard_permissions` CRUD and its
`DashboardPermissions` adapter (implementing `api/dashboards.
PermissionStore`) have real integration tests, same skip-gated shape as
§6 below:

```sh
docker run --rm --network sentry_default -v $(pwd)/enterprise:/src -w /src \
  -e RBACSTORE_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e RBACSTORE_TEST_POSTGRES_PASSWORD=sentry-dev-only \
  golang:1.25-alpine go test ./internal/rbacstore/... -run DashboardPermission -v
```

Expect all `TestDashboardPermission*`/`TestSetDashboardPermission*`/
`TestGetDashboardPermission*`/`TestRevokeDashboardPermission*`/
`TestListDashboardPermissions` tests to pass. This only takes effect
when `enterprise-api` (not plain `api`) is serving traffic -- see
§8/§10.

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
# correctly, that a request with no/invalid tenant identity is refused,
# and (since TenantChecker was added to close verification-plan item 4)
# that a tenant which exists but isn't active yet -- e.g. right after
# enterprise-auth -create-tenant but before enterprise-api
# -provision-tenant -- is refused too, not silently searched against a
# freshly-created empty index.

go test ./internal/chrunner/... -run MidProvisioning -v
# the ClickHouse half of the same item-4 probe -- also Docker-free,
# since an empty DataSource list never dials out.
```

This is the one piece of Phase 4's tenant isolation work that has
**actually been run and confirmed passing** in an environment without
Docker access, alongside `enterprise/internal/loginhandler`'s OIDC/SAML
tests (§3a/§3b) — both are unusually strong evidence precisely because
they needed no infrastructure this environment lacked. Writing the
`TenantChecker` test here is also what found the Tantivy mid-
provisioning gap in the first place, not just what closed it after the
fact -- see `api/queryapi/tenant_isolation_gap_test.go`'s item 4 for the
full story.

## 10. Confirm the Helm chart actually enforces the binary swap

No live cluster needed for this either — `helm template`'s output is
plain YAML, parseable without a cluster:

```sh
cd deploy/helm/sentry
helm template sentry . --include-crds > /tmp/default.yaml
helm template sentry . --include-crds --set enterprise.enabled=true \
  --set 'tenants[0].name=acme' --set 'tenants[0].displayName=Acme Corp' \
  > /tmp/enterprise.yaml

python3 -c "
import yaml
for f in ['/tmp/default.yaml', '/tmp/enterprise.yaml']:
    docs = list(yaml.safe_load_all(open(f)))
    deploys = [d for d in docs if d and d.get('kind')=='Deployment' and d.get('metadata',{}).get('name')=='sentry-api']
    print(f, '->', [d['spec']['template']['spec']['containers'][0]['image'] for d in deploys])
"
# expect: default.yaml -> ['sentry-api:latest'], enterprise.yaml -> ['sentry-enterprise-api:latest']
# and exactly one Deployment named sentry-api in each file.
```

Real cluster (`kind create cluster`, or similar): `helm install` with
each set of values and confirm `kubectl get deploy sentry-api -o
jsonpath='{.spec.template.spec.containers[0].image}'` matches, and that
`kubectl get svc sentry-api` routes to whichever one is actually running.

## Known gaps (do not treat this phase as done without reading these)

Full accounting: `/docs/security/threat-model.md`. Headline items:

- **Both storage engines' isolation exists, and the Helm chart now
  enforces which binary runs.** `deploy/helm/sentry/templates/api.yaml`/
  `enterprise-api.yaml` are mutually exclusive on `enterprise.enabled`
  (§10) -- a Helm-deployed cluster can't accidentally run the
  non-isolated binary once that flag is set. `docker-compose.yml` still
  runs plain `api` unconditionally alongside a separately-started
  `enterprise-api` (§8), so this enforcement doesn't extend to local/dev
  yet.
- The `Tenant` CRD (`deploy/operator`) and `enterprise-api
  -provision-tenant` are still two independent provisioning mechanisms
  -- running both for the same tenant ID today takes two separate
  operator actions, not one.
- **Ingest has no tenant concept for either storage engine.** Every
  record `ingest` produces lands in the one shared ClickHouse database
  and the one shared Tantivy index no matter what. A newly-provisioned
  tenant's storage is real, isolated at query time, and permanently
  empty until this changes — undesigned, not just unbuilt.
- **Human SSO login now works for both OIDC (§3a) and SAML (§3b)** --
  each verified with a real fake IdP (genuine cryptographic signing and
  verification), not yet a real external IdP or a running
  `enterprise-auth` container. No tenant-picker UI for a multi-membership
  identity either (refused outright) for either protocol.
- No admin UI to create a `tenant_memberships` row, but §3a/§3b's manual
  SQL bootstrap is gone -- `enterprise-auth -create-tenant`/
  `-grant-membership-*` (offline operator flags, same shape as
  `-mint-service-token`) replace it. Nothing yet for revoking a
  membership, listing a tenant's members, or changing a role after the
  fact (SetMembership's upsert supports it at the storage layer; there's
  just no flag exposing it).
- **Per-resource dashboard grants are now enforced** (`api/dashboards`'
  handler reads `dashboard_permissions` via
  `enterprise/internal/rbacstore.DashboardPermissions`, only when
  `enterprise-api` -- not plain `api` -- serves traffic), but there's
  still no UI or `sentryctl` command to create a grant -- `PUT
  /dashboards/{id}/permissions/{userId}` has to be called directly.
  Verified against a fake store; the real-Postgres integration tests
  (`enterprise/internal/rbacstore/rbacstore_test.go`) haven't run
  against a live database in this environment, same gap as the rest of
  this phase's Postgres-backed pieces.
- All four adversarial ClickHouse/Tantivy probes named in
  `/docs/phase-4-isolation-design.md`'s verification plan are now closed
  -- see `api/queryapi/tenant_isolation_gap_test.go` for the full
  accounting. The fourth (mid-provisioning-race handling) turned out to
  need a real code fix on the Tantivy side, not just a test: `search/
  src/registry.rs`'s `IndexRegistry` opened-or-created an index for any
  syntactically-valid `tenant_id`, so a mid-provisioning tenant's query
  would have silently succeeded with zero results instead of being
  refused. Fixed via `enterprise/internal/searchclient.TenantChecker`
  (backed by `rbacstore.TenantIsActive`). Both the ClickHouse and
  Tantivy halves of this probe run genuinely, without Docker, in this
  environment (`chrunner_test.go`'s
  `TestRegistryRefusesMidProvisioningTenant`, `searchclient_test.go`'s
  `TestSearchRefusesMidProvisioningTenant`) -- `rbacstore.TenantIsActive`
  itself has skip-gated live-Postgres tests that haven't run here, same
  disclosed gap as the rest of this phase's Postgres-backed pieces.

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
