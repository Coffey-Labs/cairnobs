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
of what was already run and passed. Five genuine exceptions so far:

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
- Tantivy tenant isolation, both directions -- `search/src/registry.rs`'s
  cross-tenant read isolation (§9), `search/src/consumer.rs`'s per-tenant
  write-routing (§14), and `search/src/tenants.rs`'s active-tenant gate
  (§14) -- verified live, no disclaimer needed, because Tantivy is an
  embedded library with no Docker/broker dependency, and the active-
  tenant gate's only external dependency (`enterprise-auth`'s HTTP API)
  was exercised against a real hand-rolled test server, not a live
  container: real indices, real documents, real commits, real HTTP
  requests, all run in this environment. The one thing about it that's
  still unverified is not Tantivy itself but the upstream credential/
  header plumbing feeding it (ingest's `TenantResolver`, `enterprise-auth`'s
  `/internal/authorize-ingest` and `/internal/active-tenants`) against a
  real running stack.
- The tenant-picker frontend page (§12) -- the first frontend-only piece
  in this phase exercised in a real browser rather than only
  type-checked: `web/src/routes/select-tenant`'s cross-origin
  credentialed fetch/CORS/cookie handling, driven end-to-end via
  `mcp__claude-in-chrome` against a throwaway server standing in for
  `enterprise-auth`'s exact wire contract. What's unverified is the same
  shape as OIDC/SAML above: this round trip against a real running
  `enterprise-auth` container, not a stand-in.

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
operator actions" gap named in "Known gaps" below.
`-revoke-membership-*`/`-list-memberships-tenant`/`-transfer-owner-*`
cover revoking, listing, and reassigning ownership the same way
`-grant-membership-*` covers granting (all offline operator flags, same
shape as `-create-tenant`) -- `dashboard_permissions` grants (§5a) are
the one thing here still only reachable via the HTTP endpoints
directly, no operator flag, since `sentryctl dashboards permissions
list|grant|revoke` already exists as that surface instead (§5a).

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
`curl -X POST http://localhost:8080/query` walkthrough as a logged-in
tenant is *possible* now, but still needs the manual `tenant_memberships`
bootstrap from §3a — a full end-to-end curl walkthrough isn't included
here yet.

`api`/`enterprise-api` are now mutually exclusive via `COMPOSE_PROFILES`
(checked in as `single-tenant` in `.env`, i.e. plain `api` runs by
default) — see §10a below for why, and confirmation that it actually
holds. `-provision-tenant` itself doesn't bind a port, so it runs fine
regardless of the active profile; actually serving traffic on
`enterprise-api` needs the `enterprise` profile active, since it now
binds the same host port (8080) plain `api` does:

```sh
COMPOSE_PROFILES=enterprise docker compose build enterprise-api
COMPOSE_PROFILES=enterprise docker compose run --rm enterprise-api -provision-tenant=acme -display-name="Acme Corp"
COMPOSE_PROFILES=enterprise docker compose run --rm enterprise-api -provision-tenant=globex -display-name="Globex Corporation"
COMPOSE_PROFILES=enterprise docker compose up -d enterprise-api
curl -s http://localhost:8080/healthz
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
# expect 24 tests passing, including
# registry::tests::tenant_index_is_isolated_from_default_and_other_tenants
# -- item 3 of /docs/phase-4-isolation-design.md's verification plan --
# and, since §14's write-routing pass, registry::tests::
# commit_all_commits_default_and_every_opened_tenant_index and the
# tenants:: module's real-TCP-server tests for ActiveTenantTracker
# (start_fetches_and_serves_the_initial_list,
# start_fails_closed_when_the_first_fetch_fails, etc. -- see §14).

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

## 10a. Confirm `docker-compose.yml` now enforces the same binary swap

Local/dev parity with §10 above was a named gap ("`docker-compose.yml`
still runs plain `api` unconditionally") -- closed via `COMPOSE_PROFILES`
(`api`/`enterprise-api` are each gated behind a profile, `.env` checks in
`single-tenant` as the zero-config default) plus the same "same host
port, `enterprise-api` gets a `default.aliases: [api]` network alias"
trick §10's Helm chart uses at the Service-name level. Verified in this
environment via `docker compose config` (no daemon needed -- it renders
and validates the merged YAML without starting anything):

```sh
docker compose config --quiet && echo "config is valid"

# exactly one of api/enterprise-api per profile, never both or neither:
docker compose config --services
# expect: ... api ... (no enterprise-api)
COMPOSE_PROFILES=enterprise docker compose config --services
# expect: ... enterprise-api ... (no api)

# enterprise-api really does take over api's name/port when active:
COMPOSE_PROFILES=enterprise docker compose config \
  | python3 -c "import yaml,sys,json; d=yaml.safe_load(sys.stdin)['services']['enterprise-api']; print(json.dumps({'ports': d['ports'], 'aliases': d['networks']['default']['aliases'], 'HTTP_LISTEN_ADDR': d['environment']['HTTP_LISTEN_ADDR']}, indent=2))"
# expect port 8080 (not enterprise-api's own default 8083), alias
# ["api"], and HTTP_LISTEN_ADDR ":8080"
```

`docker compose run`/`build enterprise-api` (§8's provisioning steps)
work regardless of the active profile -- explicit service references on
the command line bypass profile filtering, confirmed in this
environment (the commands got past client-side profile resolution and
failed only on `permission denied ... docker.sock`, this environment's
already-disclosed no-Docker-daemon-access limitation, not a
profile-related error). **Not verified**: an actual `docker compose up`
against a real daemon in this environment — the `config` rendering above
proves the compose file's *shape* is correct, not that containers
actually start and route traffic correctly end to end.

## 11. `Tenant` CRD unified with `-provision-tenant`

`deploy/helm/sentry/README.md`'s "Trying the two-tenant example" section
has the full `helm install` → `-provision-tenant` → `kubectl get
tenants` walkthrough. What was actually run in this environment (no
live cluster, same limitation as §10):

```sh
cd enterprise && go test ./internal/tenantcrd/... -v
# real k8s.io/client-go fake dynamic + typed clientsets, no cluster
# needed -- proves Sync() creates/updates the Tenant object and Secret
# correctly, is idempotent, never overwrites a pre-existing
# spec.displayName, and never rotates a credential across a re-sync.

cd deploy/operator && go test ./... -v
# tenant_controller_test.go rewritten for the new split: proves an
# unprovisioned tenant reports Provisioning (not Active -- the
# regression test for the pre-unification bug where this controller
# claimed Active on its own say-so), that setting
# status.clickHouseDatabaseName (simulating what -provision-tenant
# writes) flips it to Active, and that un-suspending an
# already-provisioned tenant returns straight to Active rather than
# being demoted to Provisioning.
```

Helm-side wiring confirmed via `helm template` + parsing the rendered
YAML (not eyeballing it): with `tenantOperator.enabled=true`,
`enterprise-api` gets its own ServiceAccount/Role/RoleBinding scoped to
exactly `tenants`/`tenants/status`/`secrets`, `tenant-operator`'s own
ClusterRole no longer grants `secrets` at all, and `TENANT_CRD_NAMESPACE`
is only set on `enterprise-api`'s container when `tenantOperator.enabled`
is true (absent, and the ServiceAccount/Role absent too, with just
`enterprise.enabled=true`). **Not verified**: an actual `-provision-tenant`
run against a real cluster with the operator watching -- everything
above proves each half's logic and the chart's shape independently, not
the full loop (does the operator's watch actually re-trigger a reconcile
after `-provision-tenant`'s external status write the way controller-
runtime's default predicate is expected to).

## 12. Tenant-picker, backend and frontend

**Backend** -- like §9, this needs nothing but a local Go toolchain --
the real fake-IdP tests already exercise the full login → pending-login
cookie → `GET /auth/memberships` → `POST /auth/select-tenant` → real
session round trip:

```sh
cd enterprise
go test ./internal/session/... -run PendingLogin -v
# real JWT signing/verification: issues a pending-login token, validates
# it, and proves the two negative regressions that matter most --
# a real session token must not validate as a pending login (they share
# a signing key but PendingLoginClaims uses a disjoint json field name,
# see that type's doc comment for the bug this caught in its own tests),
# and a pending-login token must not validate as a real session either.

go test ./internal/loginhandler/... -run 'Memberships|SelectTenant|MultipleMemberships' -v
# full round trip against the real fake OIDC/SAML IdPs: multi-membership
# login sets a pending cookie and redirects (not a 501 anymore), GET
# /auth/memberships lists the real tenant options with display names,
# POST /auth/select-tenant re-derives role server-side and refuses a
# tenant_id outside the identity's actual memberships.
```

**Frontend** -- `web/src/routes/select-tenant` (new), calling the
endpoints above via `fetch(..., {credentials: 'include'})`
(`$lib/api.ts`'s `listMemberships`/`selectTenant`), needed a CORS
posture `enterprise-auth` didn't have: `api/httpserver.WithCORS`'s
wildcard-friendly default can't be combined with a credentialed
request at all (browsers refuse it outright), so this needed a new
`WithCredentialedCORS` (same package, literal-origin-only) wired in via
a new `CORS_ALLOWED_ORIGIN` env var, defaulting to
`POST_LOGIN_REDIRECT_URL` (`web`'s own origin). Type-checked and built
for real:

```sh
cd web
npm run check   # svelte-check -- 0 errors
npm run build    # adapter-static -- confirms select-tenant.html is
                  # actually produced (it wasn't, at first: adapter-
                  # static only crawls routes reachable from a link or an
                  # explicit prerender entry, and nothing in the app
                  # links to this route since it's only ever reached via
                  # enterprise-auth's redirect -- fixed by adding
                  # select-tenant/+page.ts's `export const prerender =
                  # true`, the same declaration every other route here
                  # already has)
```

**Genuinely verified in a real browser in this environment** -- not just
type-checked, the actual cross-origin fetch/CORS/cookie behavior, driven
through `mcp__claude-in-chrome`:

1. A throwaway Node HTTP server (no dependencies) stood in for
   `enterprise-auth`, implementing the exact wire contract this section's
   Go tests already prove server-side: `GET /auth/memberships` and
   `POST /auth/select-tenant`, the `sentry_pending_login` cookie
   (`Path=/auth`), the credentialed CORS headers, and critically the
   *plain-text* `http.Error` response bodies the real handler sends on
   failure (not JSON -- `enterpriseAuthRequest` in `$lib/api.ts` reads
   `res.text()` specifically because of this, unlike every other request
   helper in that file).
2. `npm run dev` (SvelteKit dev server) pointed at that fake server via
   `VITE_ENTERPRISE_AUTH_BASE_URL`, both on `localhost` but different
   ports -- different origins, the same cross-origin shape a real
   deployment has.
3. The browser navigated to the fake server's `/debug/start-pending-
   login` (mimics `loginhandler.startTenantSelection`: sets the pending
   cookie, redirects to `/select-tenant`) -- confirmed the redirect
   landed on the real page, which then genuinely fetched
   `GET /auth/memberships` cross-origin *with the cookie attached* and
   rendered both fake tenants with their display names and roles.
4. Clicked a tenant in the real UI -- confirmed the real
   `POST /auth/select-tenant` fired (with preflight), succeeded, and the
   page navigated to the response's `redirect_url` via a real full page
   load.
5. Reloaded `/select-tenant` directly (no pending cookie present anymore
   -- the fake server clears it exactly like the real handler does) --
   confirmed the page's error state renders the backend's actual
   plain-text message ("missing or expired pending login...") rather
   than a generic fetch-failure string.

No console errors at any point. This is the first frontend-only piece
in this entire phase that's been exercised in a real browser rather than
only type-checked or unit-tested against fakes -- everything else
frontend-adjacent (`getAuthFeatures` on the settings page, existing
Phase 0-3 routes) predates this runbook and was never re-verified here
either.

## 13. Ingest tenant identity

The identity mechanism was chosen deliberately (config-supplied
tenant_id + a shared-secret token ingest validates, not per-tenant
mTLS certs -- smaller real implementation, no new PKI). Verified in
this environment without Docker or a live enterprise-auth, using the
same fake-client-at-every-layer discipline as everything else in this
runbook that doesn't need a live stack:

```sh
cd enterprise
go test ./internal/rbacstore/... -run IngestCredential -v
# skip-gated (RBACSTORE_TEST_POSTGRES_ADDR) -- CreateIngestCredential/
# ValidateIngestCredential/RevokeIngestCredential round trip, and the
# regression test that only a SHA-256 hash is ever persisted, never the
# plaintext token.

go test ./internal/authhandler/... -run AuthorizeIngest -v
# real HTTP round trip against POST /internal/authorize-ingest with a
# fake credential validator -- proves a session token (service or
# human) does NOT work as an ingest credential, since this endpoint
# never calls session.Manager.Validate at all.

cd ../ingest
go test ./internal/tenantresolver/... -v
# real HTTP round trip (httptest), same shape as api/authz.
# HTTPAuthorizer's own tests -- forwards the bearer token, parses
# tenant_id, treats a non-2xx or an empty tenant_id as an error.

go test ./internal/grpcserver/... -run 'Resolver|TenantHeader' -v
# PushBatch with a fake TenantResolver: no resolver configured ->
# unchanged behavior, no tenant_id header at all; resolver configured ->
# every produced Kafka message carries a tenant_id header matching the
# resolved tenant; missing or invalid bearer token -> the whole batch is
# refused (codes.Unauthenticated), fail-closed, never falls back to "no
# tenant."
```

Also not built as part of the identity mechanism itself: any Helm/
`docker-compose.yml` wiring that issues an agent a real ingest credential
automatically (`enterprise-auth -create-ingest-credential-tenant=<id>`
is, like every other credential-minting flag in this codebase, a manual
operator action) -- `deploy/helm/sentry/values.yaml`'s
`ingest.requireTenantCredential` (default `false`) only turns on
*validation*, deliberately not folded into `enterprise.enabled`
directly, since flipping that flag with no agents holding a credential
yet would refuse all ingest traffic outright rather than degrading
gracefully. See §14 below for what the identity is actually used for on
the write path.

## 14. Ingest write-routing (both storage engines)

`enterprise/cmd/enterprise-ingest` (mirrors `enterprise-api`'s "second
binary" shape) consumes the `tenant_id` Kafka header §13 attaches and
routes each record's ClickHouse write into that tenant's own database,
via `enterprise/internal/chwriter.Registry` -- a per-tenant
`*ingest/clickhousewriter.Writer` registry that reuses `ingest/
consumer`'s own flush loop unchanged. Verified in this environment
without Docker, using the same fake-transport discipline as §13:

```sh
cd enterprise
go test ./internal/chwriter/... -run 'TestWriteBatchRefuses' -v
# Docker-free: constructs a Registry directly (bypassing New(), the only
# part that dials ClickHouse) to prove the fail-closed paths -- an empty
# TenantID, or a TenantID with no registered writer, refuses the WHOLE
# batch rather than silently dropping just those records or falling back
# to a default destination. (Note: -run TestRegistry, an earlier version
# of this command, actually matches TestRegistryWritesEachTenantToItsOwnDatabase/
# TestRegistryRefusesUnprovisionedTenant instead -- the live-ClickHouse
# tests below, which just skip -- not the Docker-free ones this
# paragraph is about; caught by actually running this command while
# revisiting the runbook.)

go test ./internal/tenantprovision/... -run TestProvisionedUserCanInsertIntoOwnDatabase -v
# skip-gated (needs a live ClickHouse) -- regression test for the bug
# found while building this: the per-tenant credential chwriter reuses
# from chrunner only had SELECT granted, which would make every real
# write fail with a permission error. Fixed by granting SELECT, INSERT
# (tenantprovision.go). This test proves the fix, not just documents it,
# whenever a real ClickHouse is available to run it against.

go test ./internal/chwriter/... -run TestRegistryRoutesToCorrectTenant -v
# skip-gated (CHWRITER_TEST_CLICKHOUSE_ADDR) -- writes a mixed batch
# spanning two tenants in one WriteBatch call and confirms each row
# lands in its own tenant's database, none in the other's.
```

**Not run in this environment**: no live ClickHouse was available while
this was built, so the skip-gated tests above are correct Go that has
never actually executed -- "the test exists" is not the same claim as
"write-routing is confirmed," same caveat §8 already states for the
read-side `chrunner` tests.

**Tantivy's side is built too, and genuinely verified.**
`search/src/consumer.rs` reads the same `tenant_id` Kafka header and
resolves it through `search/src/registry.rs`'s `IndexRegistry` -- the
same registry the read side (§9) already uses -- routing each record's
write into its own tenant's Tantivy index instead of the single default
one. No "second binary" was needed here the way ClickHouse needed
`enterprise-ingest`: Tantivy has no grant system to gate a
commercially-licensed credential behind, so `IndexRegistry` already
lives directly in AGPL-core `search`, and read/write just share it.
Because Tantivy is an embedded library (no Docker/broker needed to
exercise real logic), this actually ran in this environment:

```sh
cd search
cargo test --quiet
# registry.rs: commit_all_commits_default_and_every_opened_tenant_index
# writes into the default index plus two tenant indices, confirms
# nothing is searchable before commit_all(), then confirms all three ARE
# searchable after -- the write-routing + periodic-commit path
# end-to-end, for real, using the same real-Tantivy-index discipline as
# every other registry.rs/index.rs test. consumer.rs:
# tenant_id_from_headers_* cover the header-extraction helper
# (missing/present/unrelated-header cases) and
# test_tenant_id_header_key_matches_go guards the "tenant_id" literal
# against drifting from ingest/consumer.TenantIDHeaderKey /
# ingest/internal/grpcserver.TenantIDHeaderKey the same way
# ingest/cmd/ingest's own guard test does on the Go side.
```

**Tantivy's write path is now active-tenant-gated too.**
`search/src/tenants.rs`'s `ActiveTenantTracker` polls a new
`GET /internal/active-tenants` endpoint on `enterprise-auth` (Go side:
`rbacstore.ListActiveTenantIDs` + `authhandler.handleActiveTenants`,
RoleService-credentialed -- mint one with
`enterprise-auth -mint-service-token search`, the same generic flag
`alerting` already uses, just a different subject name) and
`consumer.rs` refuses any tagged record whose tenant isn't in the polled
set. Off unless `ENTERPRISE_AUTH_URL`/`ENTERPRISE_AUTH_SERVICE_TOKEN` are
both set (`search/src/config.rs` rejects exactly one being set); startup
blocks on the first fetch succeeding (fail-closed cold start), and a
later refresh failure keeps serving the last-known-good set rather than
clearing it. Genuinely verified in this environment, no live
enterprise-auth needed:

```sh
cd search
cargo test --quiet tenants::
# start_fetches_and_serves_the_initial_list / start_sends_the_bearer_token
# run against a real (hand-rolled, dependency-free) TCP server -- actual
# reqwest request construction (the Authorization: Bearer header, the
# /internal/active-tenants path) and actual JSON response parsing, not a
# fake HTTP client. start_fails_closed_when_the_first_fetch_fails and
# start_fails_closed_when_the_server_is_unreachable prove the cold-start
# refusal: ActiveTenantTracker::start returns Err rather than falling
# back to an empty (accept-nothing, silently-safe-looking-but-wrong-for-
# operators) or permissive (accept-anything, the exact bug being closed)
# default.
go test ./internal/authhandler/... -run TestActiveTenants -v
# GET /internal/active-tenants requires a RoleService credential -- a
# valid human session (even for a real Owner) is rejected the same way
# an invalid/missing token is, the regression test for this endpoint's
# whole reason to distinguish token kinds.
```

**The ClickHouse/Tantivy asymmetry this section used to disclose is now
closed too.** `chwriter.Registry.StartRefreshing` (new) spawns a
goroutine that re-lists active tenants every minute (matching
`ActiveTenantTracker`'s interval -- see
`enterprise-ingest/main.go`'s `dataSourceRefreshInterval`) and
reconciles the writer map: opens a connection for a newly-active tenant,
closes and removes one no longer active. A tenant deprovisioned after
`enterprise-ingest` startup now loses ClickHouse write access within a
minute, not "until the next restart." A refresh failure (rbacstore
unreachable, or one tenant's new connection failing to open) logs and
leaves the existing map untouched for that tick -- the same
last-known-good posture `ActiveTenantTracker` uses, so a transient
Postgres blip doesn't evict every other tenant's already-working writer.
Neither engine does a live per-write check (a database/HTTP round trip
per record would be a real throughput cost neither implementation
accepts), so a roughly one-minute staleness window remains on both
sides by design, not a gap unique to either anymore.

```sh
cd enterprise
go test ./internal/chwriter/... -run TestRefreshListerErrorLeavesRegistryUnchanged -v
# Docker-free -- refresh's early-return on a lister error, proven the
# same way the existing fail-closed tests are: a Registry constructed
# directly (bypassing New, so nothing dials ClickHouse), asserting
# WriteBatch still refuses afterward.

go test ./internal/chwriter/... -run 'TestRefreshAddsNewlyActiveTenant|TestRefreshRemovesNoLongerActiveTenant' -v
# skip-gated (CHWRITER_TEST_CLICKHOUSE_ADDR) -- the actual add/remove
# reconciliation against real connections: a tenant absent at New() time
# gains a working writer after refresh() sees it in a later lister call;
# a tenant present at New() time loses its writer (WriteBatch starts
# refusing it) after refresh() stops seeing it.
```

**Not run in this environment**: same live-ClickHouse caveat as
everything else here -- the two new skip-gated tests are correct Go
that's never executed against a real database. See
`/docs/security/threat-model.md`'s "Read this first".

**Also not built**: Helm/`docker-compose.yml` do gate *whether*
`enterprise-ingest` runs at all (`ingest.requireTenantCredential`, same
flag §13 uses for validation -- see `deploy/helm/sentry/templates/
enterprise-ingest.yaml`), but `docker-compose.yml`'s version is a
disclosed, weaker approximation of Helm's: Helm achieves genuine
`-mode=server`/`-mode=consumer` mutual exclusivity between `ingest` and
`enterprise-ingest`; compose's `enterprise-ingest` service is a
profile-gated opt-in extra that does NOT split `ingest`'s own
server/consumer halves, so with the `enterprise` profile active, both
`ingest -mode=all` and `enterprise-ingest` independently consume every
message via different consumer groups -- harmless duplication for local
verification, not a topology compose actually enforces the way Helm
does.

## Known gaps (do not treat this phase as done without reading these)

Full accounting: `/docs/security/threat-model.md`. Headline items:

- **Both storage engines' isolation exists, and both Helm and
  docker-compose now enforce which binary runs.**
  `deploy/helm/sentry/templates/api.yaml`/`enterprise-api.yaml` are
  mutually exclusive on `enterprise.enabled` (§10) -- a Helm-deployed
  cluster can't accidentally run the non-isolated binary once that flag
  is set. `docker-compose.yml`'s `api`/`enterprise-api` services are now
  the same mutually-exclusive choice via `COMPOSE_PROFILES` (§8, §10a),
  closing the local/dev parity gap this bullet used to name.
- **The `Tenant` CRD (`deploy/operator`) and `enterprise-api
  -provision-tenant` are now unified**, in a deliberately lightweight
  way: `-provision-tenant` stays the sole real actor (ClickHouse +
  `rbacstore`) and, when `TENANT_CRD_NAMESPACE` is set (the Helm chart
  does this automatically when `tenantOperator.enabled`), also syncs the
  real result into the Tenant CRD (`enterprise/internal/tenantcrd`) --
  see §11 below. Running `-provision-tenant` is still a separate,
  deliberately manual operator action from `helm install`/`kubectl
  apply -f tenant.yaml` creating the Tenant object in the first place --
  that split (declarative request vs. imperative provisioning action)
  is intentional, not the "two disconnected sources of truth" gap this
  bullet used to describe.
- **Ingest now has a real tenant identity (§13), and both storage
  engines' write-routing is built (§14).** An agent presents a bearer
  credential (`enterprise-auth -create-ingest-credential-tenant=<id>`),
  `ingest/internal/grpcserver.TenantResolver` validates it (fail-closed)
  and attaches the resolved tenant ID to every record as a `tenant_id`
  Kafka message header. `enterprise-ingest` reads that header back and
  routes each record's ClickHouse write into its own tenant's database
  (not yet confirmed against a real ClickHouse in this environment --
  see §14). `search/src/consumer.rs` reads the same header and routes
  each record into its own tenant's Tantivy index -- genuinely verified
  in this environment, unlike the ClickHouse side, since Tantivy needs
  no Docker to exercise real logic. Both engines now also gate writes on
  an active-tenant check that refreshes every minute -- `chwriter.
  Registry.StartRefreshing` on the ClickHouse side (new, closing what
  was originally a startup-only snapshot with no refresh at all) and
  Tantivy's `ActiveTenantTracker` on the other, the same interval on
  both, no asymmetry left between them; see §14 and
  `/docs/security/threat-model.md`'s "Read this first". A
  newly-provisioned tenant's ClickHouse database and Tantivy index are
  both now real, isolated, and actually populated by write-routed
  traffic (the ClickHouse claim pending live confirmation, the Tantivy
  claim already verified) -- what used to be "permanently empty" for
  both is no longer true for either.
- **Human SSO login now works for both OIDC (§3a) and SAML (§3b)** --
  each verified with a real fake IdP (genuine cryptographic signing and
  verification), not yet a real external IdP or a running
  `enterprise-auth` container. **The tenant-picker, backend and
  frontend, is now fully built too** (§12) -- `GET /auth/memberships`/
  `POST /auth/select-tenant`, backed by a short-lived pending-login
  token distinct from a real session, and `web/src/routes/select-tenant`
  actually calls it via credentialed cross-origin `fetch`, genuinely
  exercised in a real browser against a contract-accurate fake backend
  (§12). What's still not tried is the same caveat as OIDC/SAML above:
  this whole round trip end-to-end against a real running
  `enterprise-auth` container instead of a stand-in.
- No admin UI to create a `tenant_memberships` row, but §3a/§3b's manual
  SQL bootstrap is gone -- `enterprise-auth -create-tenant`/
  `-grant-membership-*`/`-revoke-membership-*`/`-list-memberships-tenant`/
  `-transfer-owner-*` (offline operator flags, same shape as
  `-mint-service-token`) cover create/grant/revoke/list/transfer-owner.
  Changing a non-Owner role after the fact is just re-running
  `-grant-membership-*` with a different `-grant-membership-role`
  (`SetMembership`'s upsert already supports it). `RevokeMembership`
  still refuses a tenant's current Owner (would leave
  `tenants.owner_user_id` dangling), but that's no longer a dead end --
  `-transfer-owner-tenant`/`-transfer-owner-user-email`
  (`rbacstore.TransferOwner`, this package's first use of a real
  transaction: downgrades the current owner to admin, promotes the new
  owner, and updates `tenants.owner_user_id` atomically) hands ownership
  off first, and the now-downgraded former owner can be revoked
  normally after that. `-grant-membership-role=owner` itself now refuses
  when a *different* owner already exists, pointing at
  `-transfer-owner-*` instead of silently leaving a stale
  `tenant_memberships` row. Skip-gated live-Postgres tests
  (`TestTransferOwnerMovesOwnershipAndDowngradesPreviousOwner` and two
  refusal-path tests in `enterprise/internal/rbacstore/rbacstore_test.go`)
  haven't run against a live database in this environment, same gap as
  the rest of this phase's Postgres-backed pieces.
- **Per-resource dashboard grants are now enforced** (`api/dashboards`'
  handler reads `dashboard_permissions` via
  `enterprise/internal/rbacstore.DashboardPermissions`, only when
  `enterprise-api` -- not plain `api` -- serves traffic), **and now has a
  CLI surface**: `sentryctl dashboards permissions list|grant|revoke`
  (`cli/cmd/sentryctl/cmd_dashboards.go`) against
  `GET`/`PUT`/`DELETE /dashboards/{id}/permissions/{userId}` -- still no
  `web` UI for it, just the CLI. Verified against a real `httptest.
  Server` (not a fake store this time -- the CLI has no store of its
  own, just an HTTP client, so this is exercising real request
  construction/method/path/body/error-parsing, the same pattern every
  other `sentryctl` subcommand's tests use):

  ```sh
  cd cli
  go test ./... -run TestCmdDashboardsPermissions -v
  ```

  `api/dashboards`' own handler tests (fake `PermissionStore`) and the
  real-Postgres `enterprise/internal/rbacstore/rbacstore_test.go`
  integration tests are unchanged by this -- the CLI is a new caller of
  an existing, already-tested endpoint, not new server-side logic. Those
  Postgres-backed tests still haven't run against a live database in
  this environment, same gap as the rest of this phase's Postgres-backed
  pieces.
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
