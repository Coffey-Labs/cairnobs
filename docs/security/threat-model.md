# Sentry Threat Model (Phase 4)

Written for a prospective enterprise customer's security team, describing
the system **as actually built** through Phase 4 task 7 — not the target
architecture. Where a control is designed but not yet implemented, this
document says so explicitly, with a pointer to the tracking doc/task.
See `/docs/phase-4-isolation-design.md` and `/docs/phase-4-rbac-design.md`
for the full design rationale behind the controls described here.

## Read this first: the single most important open finding

**Log data queried through `POST /query` is not tenant-isolated today.**
Every authenticated tenant's ad hoc queries and dashboard panel queries
execute against the same shared ClickHouse connection and the same
shared Tantivy index — there is no per-tenant database, user, or index
routing anywhere in the query execution path
(`api/internal/querylang/executor.SQLRunner`/`SearchClient`, `search`'s
gRPC service, `proto/sentry/search/v1/search.proto`). Confirmed by
reading the actual code, not assumed: neither interface, nor the
`search` proto, carries a tenant field anywhere.

This is exactly the mechanism `/docs/phase-4-isolation-design.md`
specifies as the core deliverable of tenant isolation (one dedicated
ClickHouse database/user and one dedicated Tantivy index directory per
tenant) — it is **designed but not built**. What *is* built and live:
role-based access control (below) and tenant-scoped control-plane data
(dashboards, below). Until `enterprise/internal/chrunner` and
`enterprise/internal/searchclient` exist and are wired into
`api/internal/queryapi.Handler` in place of the single shared connection
`api/cmd/api/main.go` opens today, **treat any deployment of this system
as single-tenant only**, regardless of how many `Tenant` CRs or
`tenant_memberships` rows exist. RBAC controls who can run a query; they
do not control what data that query can see.

## System overview

```
Browser ──▶ web (SvelteKit, static)
              │
              ▼
Browser ──▶ api ──▶ ClickHouse (log data, SQL path)
              │  └─▶ search (gRPC) ──▶ Tantivy (log data, full-text path)
              └─▶ Postgres (control plane: dashboards, alert_rules,
                             tenants, users, tenant_memberships, audit_log)

alerting ──▶ api (POST /query, RoleService credential)
alerting ──▶ Postgres (rulestore, notifystore)

api/alerting ──▶ enterprise-auth (POST /internal/authorize, HTTP only —
                                    no Go import edge, see "Module
                                    boundary" below)

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
`enterprise-auth` over plain HTTP (`api/internal/authz.HTTPAuthorizer`),
forwarding only the `Cookie`/`Authorization` headers, never the full
request (`api/internal/authz/httpauthz_test.go` asserts this — an
unrelated header like `X-Forwarded-For` is never forwarded). This means
core's authorization decision is only as trustworthy as the network path
to `enterprise-auth` — see "Deployment/network assumptions" below.

## Authentication

**Not implemented for human users.** `enterprise/internal/oidc` and
`enterprise/internal/saml` wire `coreos/go-oidc`/`crewjam/saml` for the
protocol mechanics (discovery, AuthnRequest generation, token/assertion
validation), but no HTTP handler calls them — there is no
`/auth/oidc/login`, `/auth/oidc/callback`, or SAML ACS endpoint. A human
cannot log in today. `GET /auth/features` (`enterprise/internal/
authhandler`) reports whether OIDC/SAML are *configured* (for `/web`'s
settings page to conditionally render), which is independent of whether
login actually works.

**Implemented for the one machine caller.** `/alerting`'s evaluator is
the sole service-to-service caller (`POST /query`, to evaluate rule
conditions across tenants). It presents a long-lived, signed
(HS256/JWT) `RoleService` credential, minted offline via
`enterprise-auth -mint-service-token=alerting` (an operator action, not
a network-reachable endpoint) and configured via `API_SERVICE_TOKEN`.
`enterprise/internal/session.Manager` issues and validates this token;
`enterprise/internal/authhandler`'s `POST /internal/authorize` resolves
it. `RoleService` is a distinct, non-comparable lane on the `Role` type
(`api/internal/authz.Role.Satisfies`) — a service credential can never
satisfy a human-role check and vice versa, verified by exhaustive
table-driven tests (`api/internal/authz/authz_test.go`).

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
`api/internal/authz.RequireRole`/`RequireRoleOrService` calling
`enterprise-auth`. Roles: Viewer < Editor < Admin < Owner, plus the
separate `RoleService` lane above. `GET /dashboards` is Viewer+;
create/update/delete require Editor+ (`api/internal/dashboards/
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
(`api/internal/dashboards/store.go`), and the handler resolves that
tenant ID from the RBAC-authenticated identity's context
(`authz.IdentityFromContext`), **never** from a client-supplied request
field. This closes a real gap found during this document's own review:
`Dashboard.TenantID` is a JSON-tagged, client-settable field
(`api/internal/dashboards/types.go`), and the original handler/store
implementation trusted it directly on create/update and applied no
`tenant_id` filter at all on list/get/update/delete — meaning any
authenticated user could read, modify, or delete any other tenant's
dashboards simply by supplying (or guessing) their UUID, or spoof
`tenant_id` on create/import to write into a tenant they don't belong
to. Fixed as part of this task, with regression tests proving
cross-tenant access now returns 404 (not 403, which would itself leak
that the ID exists under a different tenant) —
`api/internal/dashboards/handler_test.go`'s
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
(`api/internal/queryapi/handler.go`) logs a write failure and otherwise
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
- **`system.query_log` metadata leakage** (task 2's finding): once
  per-tenant ClickHouse users exist, `system.query_log` and related
  `system.*` tables can expose other tenants' query *text* (predicate
  values, field names) even if row-level isolation between databases
  works perfectly. The design calls for revoking `system.*` access from
  every tenant user explicitly, not relying on ClickHouse's default
  template — this can only be verified once per-tenant users actually
  exist (they don't yet; see the top of this document), so it remains
  an open verification item, not a closed one.
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
| Tenant scoping on dashboards (control-plane data) | **Enforced** (fixed this task) |
| Tenant isolation on log data (`/query` → ClickHouse/Tantivy) | **Not implemented** |
| Human SSO login (OIDC/SAML) | **Not implemented** |
| Per-resource dashboard grants (`own/granted`) | **Not implemented** |
| Query audit logging (routine queries) | **Enforced**, fail-open |
| Audit log tamper detection (hash chain) | **Enforced**, verified live |
| Audit log tamper prevention (external anchoring) | **Design only** — `FileSink` is a dev stand-in |
| `system.*` ClickHouse metadata isolation | **Unverified** — depends on unbuilt per-tenant users |
| Protection against a privileged DB administrator | **Explicit non-goal** |
