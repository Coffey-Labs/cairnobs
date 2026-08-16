# Sentry Threat Model (Phase 4)

Written for a prospective enterprise customer's security team, describing
the system **as actually built** through Phase 4 task 7 — not the target
architecture. Where a control is designed but not yet implemented, this
document says so explicitly, with a pointer to the tracking doc/task.
See `/docs/phase-4-isolation-design.md` and `/docs/phase-4-rbac-design.md`
for the full design rationale behind the controls described here.

## Read this first: the single most important open finding

**Updated a seventh time — and this time the headline actually changes.**
This section originally read "log data queried through `POST /query` is
not tenant-isolated at all," then "ClickHouse is isolated but Tantivy
isn't," then "ingest tags records with a tenant identity but nothing
routes the write," then "ClickHouse write-routing is built but Tantivy's
isn't," then "both are write-routed but neither rechecks tenant-active
status live," then "both recheck, but ClickHouse's snapshot never
refreshes while Tantivy's does," then (sixth) "both engines are built
and code-complete, but the ClickHouse half has never actually run
against a real ClickHouse." That last gap is now closed: Docker access
became available, and every ClickHouse-dependent piece named below —
`tenantprovision`, `chrunner`, `chwriter`, the `system.*` metadata
isolation check, and `enterprise-api` itself actually starting and
serving traffic — has been run against a real
`clickhouse/clickhouse-server:24.8` and a real Postgres, not just
type-checked or run against fakes. Closing this loop caught and fixed
six real bugs that no amount of Docker-free testing could have found:
a Docker build-context bug that made `enterprise-auth` fail to build at
all, a validation gap in `dashboards.Store.AddPanel`/`UpdatePanel`, a
raw Postgres error leaking past `rbacstore`'s `ErrNotFound` boundary,
`enterprise-api` panicking on startup from a duplicate `GET /healthz`
route registration, ClickHouse's `default` user genuinely lacking
`CREATE USER` privilege until `CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1`
was added to `docker-compose.yml`, and — most load-bearing — a freshly
provisioned tenant user was **not** actually default-denied from
`system.*` on this ClickHouse version, exactly the risk task 2's
original design flagged as needing live verification rather than trust.
See `/docs/phase-4-runbook.md` §§1–14 for the full list of what was run
and what each finding was.

Both storage engines are isolated on both the read and write paths,
**both gate writes on an active-tenant check, and both refresh that
check periodically** — `chwriter.Registry.StartRefreshing` re-lists
active tenants every minute, the same interval
`tenants.ActiveTenantTracker` uses on the Tantivy side, opening
connections for newly-active tenants and closing/removing ones no
longer active — a deprovisioned tenant loses ClickHouse write access
within a minute, not "until the next `enterprise-ingest` restart."
Neither engine does a live per-write check (a database/HTTP round trip
per record would be a real throughput cost neither implementation
accepts), so a minute-wide staleness window remains on both sides by
design, not by oversight. What's left: two tenants (`acme`, `globex`)
have been provisioned and exercised end-to-end in this environment.
Human login itself is now verified for real — a real Auth0 identity
logged in via OIDC, selected between both tenants, and
`POST /internal/authorize` confirmed each selection issued the right
tenant/role (see §3a/§12 below) — but that walkthrough ran against plain
`api` serving `web`'s traffic, not `enterprise-api`, so the specific
combination of "real human OIDC session" and "real per-tenant ClickHouse
routing via `chrunner`" in the same request hasn't been driven end to
end yet; each half is independently confirmed (real ClickHouse
connections and Go integration tests for the routing half, real Auth0
sessions for the human-login half), just not together in one request.
SAML's human-login half still needs a real external IdP with SAML app
support (see §3b below). And **whether a given deployment actually runs
the isolated binaries** remains a deployment-time decision, not a
code-level guarantee — see below.

**ClickHouse (the SQL path) is built and now genuinely verified live.**
`enterprise/internal/tenantprovision` (real `CREATE DATABASE`/`CREATE
USER`/`GRANT`/`REVOKE`) and `enterprise/internal/chrunner` (a per-tenant
connection registry implementing `api/querylang/executor.SQLRunner`,
resolving the tenant from request identity, never a parameter) are wired
into `enterprise/cmd/enterprise-api`, which has itself been built,
started, and confirmed serving traffic on a real docker-compose stack.
Every ClickHouse-dependent test in both packages passes against a real
`clickhouse/clickhouse-server:24.8`, including the cross-tenant raw-SQL
probe and the corrected `system.*` metadata-isolation check (see "Known
residual risks" below for the real, version-specific wrinkle that check
found).

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

**Both Helm and docker-compose now close this.**
`deploy/helm/sentry/templates/api.yaml` and `enterprise-api.yaml` are
mutually exclusive, gated on opposite sides of the same
`enterprise.enabled` flag, rendering to the same Service name/port — so
a Helm-deployed cluster runs exactly one of the two binaries, chosen by
the same flag that turns on RBAC/audit/SSO, not a second
independently-forgettable decision. Verified by parsing (not
eyeballing) the rendered YAML under both values: exactly one `sentry-api`
Deployment either way, with the right image. `docker-compose.yml`'s
`api`/`enterprise-api` services are now the analogous mutually-exclusive
choice, gated behind `COMPOSE_PROFILES` (`.env` checks in
`single-tenant`, i.e. plain `api`, as the zero-config default) and
sharing the same host port/network-alias trick to stay transparent to
`alerting`/`web` either way — verified via `docker compose config`
(renders and validates the merged YAML without a daemon; confirms
`api`/`enterprise-api` never both appear in `--services` output for the
same profile selection) *and* via an actual `docker compose up` of
`enterprise-api` in this environment, which is what caught the duplicate
`GET /healthz` route panic mentioned above — see
`/docs/phase-4-runbook.md` §10a. This still only constrains
*deployment*, not *operation*: nothing stops an
operator from manually running plain `api`'s image against a cluster
(or compose project) that has tenants
provisioned, pointing at the same ClickHouse/Postgres. The Helm chart
makes the *default*, chart-managed path correct; it isn't a runtime
guard against misconfiguration.

**Ingest now has a real tenant identity, and both storage engines'
write-routing is built.** `chrunner`/`searchclient` prove *read*
isolation given tenant-scoped data exists; an optional
`ingest/internal/grpcserver.TenantResolver` closes the "does a record
know which tenant it belongs to" half by validating a per-tenant bearer
credential an agent presents
(`enterprise-auth -create-ingest-credential-tenant=<id>` mints one; only
its SHA-256 hash is ever stored) against a `POST
/internal/authorize-ingest` endpoint, and attaching the resolved tenant
ID to every record as a `tenant_id` Kafka message header before
producing it — fail-closed: once a resolver is configured, a missing or
invalid credential refuses the whole batch, never falls back to "no
tenant."

**ClickHouse**: `enterprise/cmd/enterprise-ingest` (a second binary,
mirroring `enterprise-api`) consumes that header: it reuses
`ingest/consumer`'s own flush loop with `enterprise/internal/
chwriter.Registry` — a per-tenant `*clickhousewriter.Writer` registry,
built the same way `chrunner`'s per-tenant connections are — swapped in
as the writer, so a tagged batch's records are grouped by tenant and
each group INSERTed through that tenant's own ClickHouse connection,
fail-closed on an untagged or unprovisioned tenant. Building it surfaced
a real gap in an already-shipped control: `tenantprovision.
ProvisionClickHouse`'s grant was `SELECT`-only (correct for the
read-side credential `chrunner` uses, but `chwriter` reuses the same
credential for writes) — every real per-tenant write would have failed
with a permission error until this was widened to `SELECT, INSERT`.
`Registry.StartRefreshing` (new) re-lists active tenants every minute
and reconciles the writer map — opens a connection for a newly-active
tenant, closes and removes one no longer active — closing what was
originally a startup-only snapshot with no refresh at all. **Not yet
confirmed against a real ClickHouse**, same caveat as the read-side
chrunner claim above — the Docker-free fail-closed and refresh-error
tests pass, the live-database tests (including the two new ones proving
refresh's add/remove reconciliation against real connections) are
written but skip-gated, see `/docs/phase-4-runbook.md`.

**Tantivy**: `search/src/consumer.rs` now resolves each record's
`tenant_id` header through the *same* `IndexRegistry` the read side
already used (`search/src/registry.rs`), and writes there instead of
always into the default index — genuinely verified in this environment,
same as the read-side Tantivy claim above, since Tantivy is an embedded
library with no Docker dependency. No "second binary" was needed here,
unlike ClickHouse: Tantivy has no grant system to gate a
commercially-licensed credential behind, so `IndexRegistry` already
lived directly in this AGPL-core `search` binary, and read/write simply
share it. This write path is now also active-tenant-gated:
`search/src/tenants.rs`'s `ActiveTenantTracker` polls a new
`GET /internal/active-tenants` endpoint on `enterprise-auth` every 60
seconds (RoleService-credentialed, the same auth shape `alerting` uses
against `api`) and `consumer.rs` refuses any tagged record whose tenant
isn't in the polled allowlist — fail-closed on the first fetch (startup
blocks until it succeeds), last-known-good on any later refresh failure.
Off unless `ENTERPRISE_AUTH_URL`/`ENTERPRISE_AUTH_SERVICE_TOKEN` are
both set. Genuinely verified here too: real HTTP round trips (the Bearer
header actually sent, JSON parsing, both fail-closed paths) against a
hand-rolled TCP test server, no live enterprise-auth needed since
`reqwest` doesn't care that the other end is real.

**Both engines share one open question now** — deployment topology,
covered above — and no longer differ in active-tenant staleness bound:
`chwriter.Registry.StartRefreshing` re-lists active tenants every
minute, matching `ActiveTenantTracker`'s interval, opening a connection
for a newly-active tenant and closing/removing one no longer active.
Neither is a live per-write check (that would mean a database/HTTP
round trip on every record, a real throughput cost neither
implementation accepts), so a roughly one-minute staleness window
remains on both sides by design, not a gap unique to either engine
anymore. A newly-provisioned tenant's ClickHouse database and Tantivy
index are both now real, isolated, and actually populated by
write-routed agent traffic (the ClickHouse claim pending live
confirmation, the Tantivy claim already verified).

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
search → Tantivy): `ingest` resolves and tags each record with a real
tenant ID (see "Read this first" above). `enterprise/cmd/enterprise-ingest`
consumes that tag and routes ClickHouse writes to each tenant's own
database. `search/src/consumer.rs` (Tantivy's independent Redpanda
consumer) consumes the same tag and routes each record into its own
tenant's index. Both write paths' one remaining gap — no live
active-tenant recheck (ClickHouse: a startup-time snapshot; Tantivy: no
allowlist at all) — is named in "Read this first" above, not separately
designed in `/docs/phase-4-isolation-design.md`; named here as a gap that
design doc doesn't yet cover, not just an implementation gap.

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

**Implemented for both OIDC and SAML, to the same verification bar.**
`enterprise/internal/loginhandler` serves `GET /auth/oidc/login`
(redirects to the configured IdP, with a short-lived HttpOnly cookie
carrying CSRF-protection state) and `GET /auth/oidc/callback`
(validates state, exchanges the code, verifies the ID token via
`enterprise/internal/oidc`'s real `coreos/go-oidc` wiring, upserts a
`users` row keyed by SSO subject, resolves tenant/role from
`tenant_memberships`, and issues a `session.Manager`-signed session
cookie), plus the SAML equivalent, `GET /auth/saml/login` (redirects to
the configured IdP via `enterprise/internal/saml`'s
`ServiceProvider.LoginURL`, persisting the AuthnRequest ID in a
short-lived cookie — SAML's replay/unsolicited-response defense,
standing in for OIDC's `state`) and `POST /auth/saml/acs` (validates the
assertion's signature and `InResponseTo` against that cookie via
`ServiceProvider.ParseResponse`, then converges on the same
upsert/resolve/issue-session path OIDC uses). Both are verified
end-to-end with real cryptography, not mocked: OIDC's tests spin up a
real fake IdP (`coreos/go-oidc`'s own `oidctest` package) that signs
genuine RS256 ID tokens; SAML's tests spin up a real fake IdP
(`crewjam/saml/samlidp`) that builds and signs genuine SAML assertions
and XML-signs the response, exercising the same `ServiceProvider.
ParseResponse` signature-verification path production uses. Every test
in `loginhandler_test.go` and `saml_test.go` passes, including the full
login→callback/ACS→session-cookie round trip for both protocols, and
negative-path tests for each (state/`InResponseTo` mismatch, missing/
expired credential, missing required claim, no/multiple tenant
memberships). Writing the SAML test caught two real bugs in
`enterprise/internal/saml`'s `ParseResponse`, both fixed before this
verification was considered complete: it never called `r.ParseForm()`
before reading the POSTed `SAMLResponse` field (every real ACS POST
would have decoded an empty response), and its email-attribute matching
missed `urn:oid:0.9.2342.19200300.100.1.3` (the standard LDAP "mail"
OID) — what an IdP sends by default when the SP hasn't explicitly
requested an attribute literally named "email", which is exactly what
`samlidp`'s own default assertion builder does. **Not yet verified for
either protocol**: wiring this into a running `enterprise-auth`
container against a *real* external IdP (Google/Okta/etc.) — that needs
real IdP credentials and a reachable callback/ACS URL neither of which
this environment has; see `/docs/phase-4-runbook.md`.

A user with zero `tenant_memberships` rows is refused outright (403).
More than one no longer guesses or refuses: `finishLogin` issues a
short-lived `session.Manager` "pending login" token (a distinct Go/JWT
type from a real session, with its own disjoint claim name so a real
session token can't double as one — a real bug this design's own test
suite caught before it shipped, see `session.PendingLoginClaims`'s doc
comment) and redirects to `web/src/routes/select-tenant`, backed by two
endpoints (`GET /auth/memberships`, `POST /auth/select-tenant`) that
list the identity's real tenant options and, on selection, re-derive the
role for the chosen tenant server-side (never trusting a client-supplied
role) before issuing the real session. Both the *backend protocol* and
the *frontend page* that calls it are now built. The backend is verified
with the same real-fake-IdP tests as the rest of `internal/loginhandler`.
The frontend needed a second CORS posture — `httpserver.
WithCredentialedCORS`, a literal origin plus
`Access-Control-Allow-Credentials: true`, since a credentialed `fetch`
and a wildcard `Access-Control-Allow-Origin` can never be combined, so
this couldn't reuse `enterprise-api`'s wildcard-friendly `WithCORS` — and
is genuinely verified in a real browser in this environment (not just
type-checked): the full cross-origin pending-login-cookie round trip, a
real click choosing a tenant, the post-selection redirect, and the
missing/expired-cookie error path, all driven against a throwaway server
standing in for `enterprise-auth`'s exact wire contract. `GET
/auth/features` (`enterprise/internal/authhandler`) reports whether
OIDC/SAML are *configured*, for `/web`'s settings page to conditionally
render — independent of whether a login button actually exists yet in
the UI (it doesn't yet; a user still has to be sent to
`/auth/oidc/login`/`/auth/saml/login` by some means other than clicking
something in `web`, since no page links there).

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

**Now enforced:** the RBAC matrix's `(own/granted)` qualifier for
Editor-level dashboard actions. `dashboard_permissions`
(`metadata/migrations/0024_create_dashboard_permissions.sql`, tightened
by `0033_restrict_dashboard_permissions_role.sql`) is read via
`api/dashboards.PermissionStore` — a core-defined interface, same shape
as `queryapi.AuditLogger` — implemented by
`enterprise/internal/rbacstore.DashboardPermissions` and wired in only
by `enterprise/cmd/enterprise-api`. A plain Editor may now only
edit/delete a dashboard (or its panels) they created, or one where a
grant raises their effective role to Editor; Admin/Owner still act on
any dashboard in their tenant. Managing grants themselves
(`PUT`/`DELETE /dashboards/{id}/permissions/{userId}`) is deliberately
stricter than editing content — only the creator or Admin/Owner may
grant or revoke, never a user who can edit *only* because of a grant
(closes a self-escalation path a looser check would allow). Verified by
`api/dashboards/handler_test.go`'s fake-store tests (the ownership/
grant/admin matrix, plus the granted-editor-cannot-manage-grants
regression case) — real integration tests against a live Postgres exist
in `enterprise/internal/rbacstore/rbacstore_test.go` but, like the rest
of this phase's rbacstore work, have not been run against one in this
environment. A plain `api/cmd/api` deployment with RBAC enforcement on
but no enterprise permission service wired still enforces ownership/
Admin — only the "granted" bonus and grant management are unavailable
there (nil `PermissionStore` is a documented no-op, same shape as a nil
`Authorizer`).

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
- **`system.query_log` metadata leakage — now closed and verified live**
  against the pinned version (`clickhouse/clickhouse-server:24.8`), with
  a real, non-obvious wrinkle: a freshly created tenant user was **not**
  default-denied from `system.*` the way the original design assumed
  (`ProvisionClickHouse` now issues an explicit `REVOKE SELECT ON
  system.* FROM <user>` — see that function's doc comment). Confirming
  this live also surfaced a genuine ClickHouse behavioral split the
  design doc didn't anticipate: `system.query_log` is a real
  access-checked table (the REVOKE makes it hard-deny,
  `ACCESS_DENIED`), but `system.tables` is a filtered *catalog* view
  that ClickHouse 24.8 never denies outright regardless of grants — it
  just silently returns zero rows for a properly-revoked user. Both
  outcomes close the actual leak (no other tenant's query text or
  database/table names are visible either way);
  `tenantprovision_test.go`'s `TestProvisionedUserCannotReadSystemTables`
  was corrected to assert what each table actually does (hard error for
  `query_log`, verified-empty-and-no-foreign-database-names for
  `tables`) rather than demanding a hard error from both. Still
  contingent on the deployment-shape caveat at the top of this document:
  this only holds when `enterprise-api` (not plain `api`) is actually
  serving traffic.
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
| ClickHouse per-tenant provisioning (`tenantprovision`) | **Enforced, verified live** against `clickhouse/clickhouse-server:24.8` — real `CREATE DATABASE`/`CREATE USER`/`GRANT`/`REVOKE`, real two-tenant cross-read probe |
| ClickHouse query routing (`chrunner`) | **Enforced, verified live** — real per-tenant connections, cross-tenant raw-SQL probe passes; only applies when `enterprise-api` serves traffic, not plain `api` |
| `system.*` ClickHouse metadata isolation | **Enforced, verified live** — `system.query_log` hard-denies, `system.tables` returns zero foreign rows (see "Known residual risks" below for the ClickHouse-version-specific split between the two) |
| Tantivy per-tenant index routing (`search/src/registry.rs`) | **Enforced, verified live** — real Tantivy indices, real cross-tenant probe, all passing |
| Tantivy tenant_id resolution (`enterprise/internal/searchclient`) | **Enforced, verified live** — real gRPC wire-level test |
| Ingest tenant *identity* (credential validation, tagging) | **Built and tested** — fail-closed `TenantResolver`, `tenant_id` Kafka header attached per record |
| Ingest tenant *write-routing*, ClickHouse | **Enforced, verified live** — `enterprise-ingest`/`chwriter.Registry` route each tagged batch to its tenant's own database, fail-closed on an untagged/unprovisioned tenant; both Docker-free and live-ClickHouse tests pass. Active-tenant snapshot refreshes every minute (`Registry.StartRefreshing`) — a deprovisioned tenant loses write access within a minute, not "until the next restart" |
| Ingest tenant *write-routing*, Tantivy | **Built and genuinely verified** — `search/src/consumer.rs` routes each record into its own tenant's index via `IndexRegistry`, same registry the (already-verified) read side uses; no Docker needed, real tests pass. Active-tenant-gated too: `tenants::ActiveTenantTracker` polls `enterprise-auth` every 60s (off unless configured), refusing any tenant not in the polled allowlist — same one-minute staleness bound as ClickHouse's now-refreshing snapshot, no more asymmetry between the two |
| Deployment actually routing traffic to `enterprise-api` (Helm) | **Enforced** — `api`/`enterprise-api` are mutually exclusive, same flag as RBAC/audit/SSO |
| Deployment actually routing traffic to `enterprise-api` (docker-compose) | **Enforced, verified live** — `api`/`enterprise-api` are mutually exclusive via `COMPOSE_PROFILES`, same flag choice as Helm's `enterprise.enabled`; a real `docker compose up` of `enterprise-api` was run in this environment (and caught/fixed a startup-crashing duplicate `GET /healthz` route registration bug in the process), not just `docker compose config` |
| Human SSO login — OIDC | **Enforced, verified live** — real login against a real Auth0 developer tenant, full browser round trip; correctly failed closed on an identity with no `tenant_memberships` row, then succeeded and issued a real session after `-grant-membership-*`, with `POST /internal/authorize` returning exactly the granted tenant/role |
| Human SSO login — SAML | **Built, verified with a real fake IdP** (not yet tried against a real external IdP) |
| Multi-tenant-membership login (tenant picker) | **Enforced, verified live** — a real Auth0 identity with two real tenant memberships (`acme` Admin, `globex` Viewer) landed on the real `/select-tenant` page against the real `enterprise-auth` container, rendered both with correct display names/roles via a real credentialed cross-origin `GET /auth/memberships`, and selecting either one issued a session that `POST /internal/authorize` confirmed matched — the selection genuinely determines the issued session's tenant, not just renders correctly. This pass also found and fixed a real bug: `web/Dockerfile` never declared `ARG`/`ENV` for `VITE_ALERTING_API_BASE_URL`/`VITE_ENTERPRISE_AUTH_BASE_URL`, so `docker-compose.yml`'s build args for them were silently dropped, leaving `enterpriseAuthBase` `undefined` in the built bundle |
| Per-resource dashboard grants (`own/granted`) | **Enforced, verified live** — real Postgres integration tests for `dashboard_permissions` CRUD and the `PermissionStore` adapter all pass (only when `enterprise-api` serves traffic — plain `api` falls back to own/Admin only) |
| Query audit logging (routine queries) | **Enforced**, fail-open, and now wired to a real writer via `enterprise-api` (`audit.QueryAPILogger`) |
| Audit log tamper detection (hash chain) | **Enforced**, verified live |
| Audit log tamper prevention (external anchoring) | **Design only** — `FileSink` is a dev stand-in |
| Mid-provisioning-race handling (evaluator ticks against a not-yet-active tenant) | **Closed on both storage engines** — see `api/queryapi/tenant_isolation_gap_test.go`; ClickHouse verified Docker-free (structural, not just tested), Tantivy fixed and verified Docker-free after finding it was a real gap, not just an unverified assumption |
| Protection against a privileged DB administrator | **Explicit non-goal** |
