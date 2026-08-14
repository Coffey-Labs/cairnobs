# Sentry Threat Model (Phase 4)

Written for a prospective enterprise customer's security team, describing
the system **as actually built** through Phase 4 task 7 — not the target
architecture. Where a control is designed but not yet implemented, this
document says so explicitly, with a pointer to the tracking doc/task.
See `/docs/phase-4-isolation-design.md` and `/docs/phase-4-rbac-design.md`
for the full design rationale behind the controls described here.

## Read this first: the single most important open finding

**Updated a second time.** This section originally read "log data
queried through `POST /query` is not tenant-isolated at all," then
"ClickHouse is isolated but Tantivy isn't." Both ClickHouse *and*
Tantivy connection/index-layer isolation are now built. What's left is
narrower but still real: **whether a given deployment actually runs the
isolated binary**, and **whether ingest itself is tenant-aware** (it
isn't, for either storage engine).

**ClickHouse (the SQL path) is built.** `enterprise/internal/
tenantprovision` (real `CREATE DATABASE`/`CREATE USER`/`GRANT`) and
`enterprise/internal/chrunner` (a per-tenant connection registry
implementing `api/querylang/executor.SQLRunner`, resolving the tenant
from request identity, never a parameter) are wired into
`enterprise/cmd/enterprise-api`. **Not yet confirmed against a real
ClickHouse** — this environment had no Docker/database access while
these were written; the tests exist and are correct Go, but "the test
exists" is not the same claim as "isolation is confirmed" (see
`/docs/phase-4-runbook.md`).

**Tantivy (the free-text path) is also built, and — unlike the
ClickHouse pieces — genuinely verified in this environment.**
`search/src/registry.rs`'s `IndexRegistry` resolves a `SearchRequest.
tenant_id` to its own on-disk Tantivy index, opened on demand;
`enterprise/internal/searchclient` sets that field from the
authenticated request identity, mirroring `chrunner`'s exact "read from
ctx, fail closed, never a parameter" shape. Because Tantivy is an
embedded library (no external service to fake or skip), both sides could
actually be run: `search/src/registry.rs`'s
`tenant_index_is_isolated_from_default_and_other_tenants` seeds three
real indices with the same search term and confirms a tenant-scoped
search returns only that tenant's document; `enterprise/internal/
searchclient`'s tests run a real in-process gRPC server and confirm the
wire-level `SearchRequest` carries the right `tenant_id`. All pass, for
real, no disclaimer needed for this specific claim.

**But plain `api/cmd/api` still runs with one shared ClickHouse
connection and no tenant-scoped search client**, and nothing in this
repo automatically routes traffic to `enterprise-api` instead —
`docker-compose.yml` includes it "available, not defaulted into the
traffic path" (same shape as `enterprise-auth`'s own addition), and the
Helm chart has no service for it at all yet. **A deployment is only as
isolated as which binary is actually serving traffic** — this is an
operational decision nothing currently enforces or even surfaces as a
warning. This is now the single largest gap in the isolation story, not
a missing mechanism.

**Ingest is not tenant-aware for either storage engine**, and this is
more load-bearing than it sounds: `chrunner`/`searchclient` prove *read*
isolation given tenant-scoped data exists, but nothing writes
tenant-scoped data yet. Every record `ingest` produces lands in the one
shared ClickHouse database and the one shared (default) Tantivy index,
regardless of tenant. A newly-provisioned tenant's ClickHouse database
and Tantivy index are real, isolated, and queryable through
`enterprise-api` — and permanently empty, until ingest itself becomes
tenant-aware, which is undesigned, not just unbuilt.

## System overview

```
Browser ──▶ web (SvelteKit, static)
              │
              ▼
Browser ──▶ api OR enterprise-api ──▶ ClickHouse (log data, SQL path)
              │                    └─▶ search (gRPC) ──▶ Tantivy (log data, full-text path)
              └─▶ Postgres (control plane: dashboards, alert_rules,
                             tenants, users, tenant_memberships, audit_log)

# api: one shared ClickHouse connection, one shared (default) Tantivy
# index via api/searchclient, nil AuditLogger -- Phase 0-3 behavior.
# enterprise-api: enterprise/internal/chrunner (per-tenant ClickHouse
# connections) + enterprise/internal/searchclient (per-tenant Tantivy
# index, via search's SearchRequest.tenant_id) + enterprise/internal/
# audit.QueryAPILogger (real audit writes) wired into the SAME
# api/queryapi.Handler/api/dashboards.Handler core -- see this
# document's "Read this first" section. Either binary can be running;
# nothing forces the isolated one.

alerting ──▶ api or enterprise-api (POST /query, RoleService credential)
alerting ──▶ Postgres (rulestore, notifystore)

api/alerting ──▶ enterprise-auth (POST /internal/authorize, HTTP only —
                                    no Go import edge, see "Module
                                    boundary" below)

Browser ──▶ enterprise-auth (GET /auth/oidc/login, /auth/oidc/callback)
              └─▶ external IdP (OIDC authorization code flow)
              └─▶ Postgres (rbacstore: users, tenant_memberships)

sentryctl ──▶ api, alerting (Bearer token when SENTRYCTL_TOKEN is set)
```

Ingest path (agent → Redpanda → ingest → ClickHouse, and Redpanda →
search → Tantivy) carries no tenant concept at all yet either — every
ingested log record lands in the one shared `logs` table/index. Tenant
isolation for *ingest*, not just query, is out of scope for what's built
so far and is not separately designed in
`/docs/phase-4-isolation-design.md`; named here as a gap that design doc
doesn't yet cover, not just an implementation gap.

## Module boundary (trust boundary #1)

`enterprise/` (commercial license: SSO, RBAC storage, audit logging,
session issuance) is never imported by AGPL core (`/api`, `/alerting`,
`/web`, `/cli`) — enforced in CI by `hack/check-tenant-boundary.sh`,
which greps for the import edge on every build. Core calls
`enterprise-auth` over plain HTTP (`api/authz.HTTPAuthorizer`),
forwarding only the `Cookie`/`Authorization` headers, never the full
request (`api/authz/httpauthz_test.go` asserts this — an
unrelated header like `X-Forwarded-For` is never forwarded). This means
core's authorization decision is only as trustworthy as the network path
to `enterprise-auth` — see "Deployment/network assumptions" below.

## Authentication

**Implemented for OIDC, still missing for SAML.**
`enterprise/internal/loginhandler` serves `GET /auth/oidc/login`
(redirects to the configured IdP, with a short-lived HttpOnly cookie
carrying CSRF-protection state) and `GET /auth/oidc/callback`
(validates state, exchanges the code, verifies the ID token via
`enterprise/internal/oidc`'s real `coreos/go-oidc` wiring, upserts a
`users` row keyed by SSO subject, resolves tenant/role from
`tenant_memberships`, and issues a `session.Manager`-signed session
cookie). Verified end-to-end with real cryptography, not mocked: the
tests spin up a real fake IdP (`coreos/go-oidc`'s own `oidctest`
package) that signs genuine RS256 ID tokens, and
`enterprise/internal/loginhandler`'s handler verifies them for real via
the same code path production uses — every test in
`loginhandler_test.go` passes, including the full login→callback→
session-cookie round trip. **Not yet verified**: wiring this into a
running `enterprise-auth` container against a *real* external IdP
(Google/Okta/etc.) — that needs real IdP credentials and a reachable
callback URL neither of which this environment has; see
`/docs/phase-4-runbook.md`.

A user with zero or more than one `tenant_memberships` row is refused
outright (403 / 501 respectively) rather than guessed at — a
tenant-selection UI for the multi-membership case is real, undesigned
future work, not silently approximated. `enterprise/internal/saml` still
only does the protocol mechanics (AuthnRequest generation, assertion
validation) with no ACS HTTP handler calling it — SAML login remains
unimplemented, following `loginhandler`'s OIDC pattern once it is built.
`GET /auth/features` (`enterprise/internal/authhandler`) reports whether
OIDC/SAML are *configured*, for `/web`'s settings page to conditionally
render — independent of whether a login button actually exists yet in
the UI (it doesn't; only the two HTTP endpoints do).

**Implemented for the one machine caller.** `/alerting`'s evaluator is
the sole service-to-service caller (`POST /query`, to evaluate rule
conditions across tenants). It presents a long-lived, signed
(HS256/JWT) `RoleService` credential, minted offline via
`enterprise-auth -mint-service-token=alerting` (an operator action, not
a network-reachable endpoint) and configured via `API_SERVICE_TOKEN`.
`enterprise/internal/session.Manager` issues and validates this token;
`enterprise/internal/authhandler`'s `POST /internal/authorize` resolves
it. `RoleService` is a distinct, non-comparable lane on the `Role` type
(`api/authz.Role.Satisfies`) — a service credential can never
satisfy a human-role check and vice versa, verified by exhaustive
table-driven tests (`api/authz/authz_test.go`).

**Session/token integrity.** Tokens are HS256-signed JWTs with a single
shared signing key (`ENTERPRISE_SESSION_SIGNING_KEY`, ≥32 bytes,
required at `enterprise-auth` startup). Compromise of this key lets an
attacker forge any identity, including `RoleService` — it is the single
highest-value secret in the enterprise deployment and should be treated
accordingly (a real KMS/secrets-manager-backed value, not the
`docker-compose.yml`/Helm chart's dev-only literal). Token validation
(`enterprise/internal/session.Manager.Validate`) collapses every failure
mode — bad signature, malformed token, expired — into one
`ErrInvalidToken`, deliberately not distinguishing "expired" from
"forged" so a caller can't be tempted to treat either as a softer case.

## Authorization (RBAC)

**Live and enforced.** `POST /query` and every `/dashboards` endpoint in
`api` require a minimum role, resolved per-request via
`api/authz.RequireRole`/`RequireRoleOrService` calling
`enterprise-auth`. Roles: Viewer < Editor < Admin < Owner, plus the
separate `RoleService` lane above. `GET /dashboards` is Viewer+;
create/update/delete require Editor+ (`api/dashboards/
handler.go`). A nil `Authorizer` (no `ENTERPRISE_AUTH_URL` configured)
is a deliberate no-op, matching Phase 0-3's no-auth behavior — this is
correct default-open-for-single-tenant behavior, not an oversight, but
means an operator who forgets to set `ENTERPRISE_AUTH_URL` in a
multi-tenant deployment gets *no* enforcement at all, silently. Worth a
deployment-time check a real rollout should add (not built here).

**Not yet enforced:** the RBAC matrix's `(own/granted)` qualifier for
Editor-level dashboard actions — `dashboard_permissions` (per-resource
grants beyond a user's tenant-baseline role) has a schema
(`metadata/migrations/0024_create_dashboard_permissions.sql`) but no
handler reads it yet. Every Editor in a tenant can act on every
dashboard in that tenant, not just their own/granted ones.

**Application-layer tenant scoping (dashboards only).** Every
`dashboards` store query filters `WHERE tenant_id = $identity.TenantID`
(`api/dashboards/store.go`), and the handler resolves that
tenant ID from the RBAC-authenticated identity's context
(`authz.IdentityFromContext`), **never** from a client-supplied request
field. This closes a real gap found during this document's own review:
`Dashboard.TenantID` is a JSON-tagged, client-settable field
(`api/dashboards/types.go`), and the original handler/store
implementation trusted it directly on create/update and applied no
`tenant_id` filter at all on list/get/update/delete — meaning any
authenticated user could read, modify, or delete any other tenant's
dashboards simply by supplying (or guessing) their UUID, or spoof
`tenant_id` on create/import to write into a tenant they don't belong
to. Fixed as part of this task, with regression tests proving
cross-tenant access now returns 404 (not 403, which would itself leak
that the ID exists under a different tenant) —
`api/dashboards/handler_test.go`'s
`TestCrossTenant*`/`TestCreateDashboardIgnoresClientSuppliedTenantID`/
`TestImportIgnoresExportedTenantID`. **This same class of bug should be
assumed present anywhere else client-supplied identifiers cross a tenant
boundary until proven otherwise by an adversarial test** — see task 8's
adversarial test suite for what's been checked so far and what hasn't.

**Query-path tenant scoping: none** — see the top of this document.
RBAC's role check on `POST /query` answers "is this identity allowed to
run *a* query," not "does this query's result set respect tenant
boundaries" — it can't, because the executor has no tenant concept to
enforce.

## Audit logging

**Live**, and independently verified against a real Postgres (not just
written) — `enterprise/internal/audit`'s integration tests. Two
independent defenses back "no update/delete path from the application
layer":

1. A dedicated `audit_writer` Postgres role with only `INSERT`+`SELECT`
   grants (`metadata/migrations/0012-0014`), via its **own**
   `pgxpool.Pool` — never the shared `sentry` role/pool every other
   store uses.
2. A `BEFORE UPDATE OR DELETE ... RAISE EXCEPTION` trigger
   (`metadata/migrations/0015-0016`) that rejects the operation for
   *any* role, including the table owner — confirmed live: even the
   `sentry` role cannot `UPDATE` a row without first disabling the
   trigger, a privileged operation distinct from ordinary application
   access.

**Tamper detection, not tamper prevention against a privileged
attacker.** Rows are hash-chained (`prev_hash`/`row_hash =
SHA256(prev_hash || canonical_fields)`, serialized under
`pg_advisory_xact_lock` so concurrent writers can't fork the chain —
verified with a 20-goroutine concurrency test against live Postgres).
The chain alone only proves internal self-consistency: a Postgres
superuser (or anyone who compromises that credential) can wipe
`audit_log` and regenerate a perfectly self-consistent new chain from
row 1. `enterprise/internal/audit.Checkpointer` periodically ships a
rolling hash to an external `CheckpointSink` for exactly this reason —
`FileSink` (the only implementation built so far) is explicitly
documented as a dev/testing stand-in, **not** a real external-anchoring
guarantee (it writes to a local file the same privileged attacker could
also reach). A real deployment needs a genuine `CheckpointSink`
(S3 with Object Lock, or equivalent, reachable by a credential the
database administrator doesn't also hold) before the "prove nothing was
altered after the fact" claim actually holds against a privileged
insider.

**Fail-open by design for routine queries.** `queryapi.Handler.logAudit`
(`api/queryapi/handler.go`) logs a write failure and otherwise
ignores it — an audit-log outage does not take down the query path. This
is a deliberate availability-over-completeness tradeoff: it means a
brief audit outage produces an under-logged (not over-blocked) window.
No privileged/administrative action (role change, SSO config change,
notification-target secret reveal) currently exists to enforce
fail-closed on, since none of those flows are built yet
(`enterprise/internal/rbacstore` has no HTTP handlers) — when they are,
they should fail closed per `/docs/phase-4-isolation-design.md`'s
original policy, and that policy is not yet exercised by any real code
path.

**What's logged:** query text, language, row count, duration,
success/error — not result contents. `Source`/`EventType` fields exist
(`SourceAPI`/`SourceWeb`/`SourceCLI`/`SourceAlerting`,
`EventQuery`/`EventRoleChange`/`EventGrantChange`/
`EventSSOConfigChange`/`EventSecretReveal`) but only `EventQuery` from
`SourceAPI` is actually wired to a call site
(`queryapi.Handler.logAudit`) — the others are typed placeholders for
work not yet built (there's no role-change/grant-change/SSO-config
handler to call them from).

## Known residual risks (explicitly out of scope, not silently assumed away)

Per `/CLAUDE.md`'s Phase 4 non-goals, restated here in threat-model
terms:

- **A privileged ClickHouse/Postgres administrator is not defended
  against.** Every isolation and audit-integrity guarantee in this
  document is a structural defense against *application-layer* bugs and
  injection — not against someone holding database superuser
  credentials. That's an operational control (credential custody,
  infrastructure access review), out of scope for this system's own
  code.
- **`system.query_log` metadata leakage — per-tenant users are now
  real, but the check itself hasn't run yet.** Was an open verification
  item because there were no per-tenant ClickHouse users to check
  against; that blocker is gone (`enterprise/internal/tenantprovision`
  exists), and `tenantprovision_test.go`'s
  `TestProvisionedUserCannotReadSystemTables` asserts exactly what the
  design calls for (`system.query_log`/`system.tables` inaccessible,
  `SHOW DATABASES` not revealing other tenants) — but this environment
  never had ClickHouse access to actually run it, so it remains
  unconfirmed against the pinned version
  (`clickhouse/clickhouse-server:24.8`) until someone with Docker access
  runs it (`/docs/phase-4-runbook.md` §8). Also still contingent on the
  deployment-shape caveat at the top of this document: even once
  confirmed, this only holds when `enterprise-api` (not plain `api`) is
  actually serving traffic.
- **No deny-override grants** — `dashboard_permissions` is additive-only
  by design; a full allow/deny ACL system is unbuilt, future work.
- **No data retention/deletion policy** for a deprovisioned tenant —
  the `tenants.status` state machine includes `deprovisioning`, but what
  actually happens to that tenant's ClickHouse/Tantivy/Postgres data is
  an unanswered compliance question, not a designed-and-deferred one.
- **No general multi-cluster orchestration** — `/deploy`'s Helm
  chart/Operator (`/deploy/README.md`) proves the K8s-side per-tenant
  secret-management model, not a fully general multi-cluster system, and
  was never applied to a live cluster in this environment (see that
  README's verification section).

## Deployment/network assumptions

- `enterprise-auth`'s `/internal/authorize` and `/auth/features`
  endpoints have no authentication of their own beyond the credentials
  they're validating — they must be reachable only from inside the
  cluster/trusted network (`api`/`alerting`/`web`), never exposed
  publicly. Nothing in this codebase enforces that at the network layer;
  it's a deployment responsibility (NetworkPolicy, or equivalent) not
  yet codified in `/deploy/helm/sentry`.
- `ENTERPRISE_SESSION_SIGNING_KEY`, ClickHouse/Postgres passwords, and
  (once minted) the `alerting` service token are all K8s `Secret`
  objects in the Helm chart (`/deploy/helm/sentry/templates/
  secrets.yaml`) — standard K8s `Secret` semantics apply (base64, not
  encrypted at rest without a cluster-level `EncryptionConfiguration`).
  No secrets-manager integration (Vault, cloud KMS) exists; the chart
  documents this as an operator decision, not something it enforces.

## Summary: what's actually enforced today

| Control | Status |
|---|---|
| Role-based access control on `/query`, `/dashboards` | **Enforced** |
| `alerting`↔`api` service-identity credential | **Enforced** |
| Tenant scoping on dashboards (control-plane data) | **Enforced** |
| ClickHouse per-tenant provisioning (`tenantprovision`) | **Built, not live-verified** — real integration test exists, not yet run against ClickHouse |
| ClickHouse query routing (`chrunner`) | **Built, not live-verified** — and only applies when `enterprise-api` serves traffic, not plain `api` |
| `system.*` ClickHouse metadata isolation | **Built, not live-verified** — same caveat as above |
| Tantivy per-tenant index routing (`search/src/registry.rs`) | **Enforced, verified live** — real Tantivy indices, real cross-tenant probe, all passing |
| Tantivy tenant_id resolution (`enterprise/internal/searchclient`) | **Enforced, verified live** — real gRPC wire-level test |
| Ingest tenant-awareness (ClickHouse and Tantivy both) | **Not implemented, undesigned** — every ingested record lands in the single shared database/index regardless of tenant |
| Deployment actually routing traffic to `enterprise-api` | **Not implemented** — no Helm service, no default wiring; now the largest gap in the isolation story |
| Human SSO login — OIDC | **Built, verified with a real fake IdP** (not yet tried against a real external IdP) |
| Human SSO login — SAML | **Not implemented** |
| Multi-tenant-membership login (tenant picker) | **Not implemented** — refused with a clear error, not guessed |
| Per-resource dashboard grants (`own/granted`) | **Not implemented** |
| Query audit logging (routine queries) | **Enforced**, fail-open, and now wired to a real writer via `enterprise-api` (`audit.QueryAPILogger`) |
| Audit log tamper detection (hash chain) | **Enforced**, verified live |
| Audit log tamper prevention (external anchoring) | **Design only** — `FileSink` is a dev stand-in |
| Mid-provisioning-race handling (evaluator ticks against a not-yet-active tenant) | **Unverified** — see `api/queryapi/tenant_isolation_gap_test.go` |
| Protection against a privileged DB administrator | **Explicit non-goal** |
