# Project: Sentry — Distributed Log Aggregation & Observability Platform

## Mission
Build an open-core, Kubernetes-native centralized logging platform that rivals
Splunk on features but wins on cost-per-GB, modern language stack, and honest
multi-tenant RBAC. Full architecture spec is in `/docs/architecture.md` — read
it before touching any component. Do not deviate from the storage/query split
described there without flagging it to me first.

## Non-negotiable constraints
- Distro-agnostic Linux agent: must run identically on RHEL/Debian/Arch/SUSE
  derivatives via a statically-linked musl binary. No glibc runtime deps.
- Windows support via native ETW/Event Log API, not a WSL shim.
- AGPLv3 for core + agents. Enterprise module (SSO/multi-tenancy/compliance)
  lives in a separate `enterprise/` directory under a commercial license stub
  — keep the boundary clean from day one, don't let AGPL code import from it.
- Schema-on-write with OTel semantic conventions as the default schema, with
  schema-on-read fallback for unstructured text.
- Every UI action must correspond to a documented REST/gRPC call. No
  UI-only logic. CLI (`sentryctl`) and Terraform provider are first-class,
  not afterthoughts.

## Tech stack (pinned — do not substitute without discussion)
| Component        | Language/Tool          |
|-------------------|------------------------|
| Edge agent        | Rust, musl target       |
| Transport         | Redpanda (Kafka API)    |
| Ingest/parse      | Go                      |
| Analytical store  | ClickHouse              |
| Full-text index   | Tantivy (Rust)          |
| Control plane/API | Go, gRPC + REST gateway |
| Frontend          | SvelteKit + TypeScript  |
| Deployment         | Kubernetes Operator (Go, kubebuilder), Helm, docker-compose for local/homelab |

## Repo conventions
- Monorepo, one top-level dir per component (see structure below).
- Rust: workspace-based, `cargo clippy --all-targets -- -D warnings` must pass.
- Go: standard `go vet` + `golangci-lint`, no globals for shared state.
- Every component ships with: unit tests, a `README.md`, and a Dockerfile
  using distroless or scratch base images where feasible.
- Conventional commits. Every PR-sized change should be a logically complete,
  independently revertible unit.
- Prefer boring, well-understood dependencies over novel ones. This is
  infrastructure software; operators need to trust it.

## What "done" looks like for Phase 0 (MVP)

**Status: shipped.** A single log line, generated on a Linux host by the
Rust agent, flows: agent → Redpanda → Go ingest service → ClickHouse, and
is queryable via a minimal SQL endpoint and visible in a bare-bones
SvelteKit table view. Verified end-to-end on real hardware, not just in
CI — see `/docs/phase-0-runbook.md`. No alerting, no multi-tenancy, no
dashboards — that discipline held for the whole phase.

## What "done" looks like for Phase 1

**Status: shipped.** A Windows Event Log entry and a Linux journald entry
are both queryable via SQL (the ClickHouse path) and via free-text search
(the Tantivy path), from the same UI, within a few seconds of being
generated. Verified end-to-end on the live stack, including the same
`record_id` coming back from both query paths for the same record — see
`/docs/phase-1-runbook.md`.

ETW and WEF (Windows Event Forwarding) were *designed* in this phase but
not required to be running for "done": ETW ships behind a feature flag
most environments won't enable (it needs elevated privileges), and WEF's
receiver-side was explicitly deferred rather than built. Only the Event
Log source needed to actually be running end-to-end, and did. The
Windows-specific agent code itself (`EvtSubscribe`, ETW, service
registration) remains unverified on real Windows — no Windows toolchain
existed anywhere in the environment this was built in; flagged
prominently in `/agent/README.md` and the runbook.

## What "done" looks like for Phase 2

A single query bar in the web UI and a single `sentryctl query` command
can express filter + free-text + stats in one query (e.g. `service=api |
where status>=500 | stats count by host | sort -count`, or
`message:"connection refused" | stats count by host`), execute correctly
against both ClickHouse and Tantivy in one compiled plan, and return in
well under a second for a 1M-row fixture dataset (rough benchmark, not a
formal SLA — see `/docs/phase-2-runbook.md` for the actual measurement).
Raw ClickHouse SQL remains available as an escape hatch, compiling to the
same execution plan/IR as the pipe syntax so performance doesn't depend
on which syntax a query uses.

Non-goals for this phase (same "resist scope creep" discipline as every
phase so far): no alerting, no dashboards, no multi-tenancy — this phase
is the query layer only. The two separate placeholder pages/endpoints
from Phase 0/1 (`/query` raw-SQL-only, `/search` free-text-only) are
retired, replaced by one `/query` endpoint and one query page.

See `/docs/query-language-design.md` for the grammar, IR, and
ClickHouse/Tantivy routing strategy, and
`/docs/query-language-reference.md` for the user-facing syntax reference
once built.

## What "done" looks like for Phase 3

**Status: shipped.** A user can build a multi-panel dashboard from saved Phase 2 queries (at
least a line chart panel and a table panel, working end-to-end against
live data), save an alert rule that fires a Slack webhook when a
condition is met (threshold comparison, or "absence" — the query returned
zero rows in its own time window), and see the delivery attempt logged —
all from the web UI, without touching the API directly. See
`/docs/phase-3-dashboard-design.md` and `/docs/phase-3-alerting-design.md`
for the data models and the alerting evaluator's firing/resolved state
machine, and `/docs/phase-3-runbook.md` for the live-stack verification,
including a load test of the alert evaluator against ~500 concurrent
rules.

This phase adds PostgreSQL as a new pinned-stack component (see the
dashboard design doc for why ClickHouse can't do this job — dashboards
and alert state need real row-level locking and transactional
read-modify-write, which ClickHouse's MergeTree family doesn't provide),
scoped strictly to control-plane config: dashboards, panels, notification
targets, alert rules, alert state, delivery log. Log data itself stays on
ClickHouse/Tantivy only, unchanged.

Non-goals for this phase (same discipline as every phase so far):
- No multi-tenancy enforcement and no `enterprise/` module work — single
  tenant/org assumed. Most new tables (`dashboards`, `alert_rules`,
  `notification_targets`) carry a `tenant_id` column so part of Phase 4's
  retrofit doesn't require a migration + backfill — but `alert_state` and
  `delivery_log` do not (an inconsistency found during Phase 4 planning,
  not caught at the time); Phase 4 adds `tenant_id` to those two and
  backfills via a join through `alert_rules.id`, and — per
  `/docs/phase-4-isolation-design.md` — tenant isolation itself turned
  out to live at the ClickHouse/Tantivy connection layer, not via these
  columns at all, since Phase 2's raw-SQL escape hatch can never be
  covered by a row filter regardless of which tables carry one.
- No raw-SQL dashboard panels (time-range injection isn't reliable
  against arbitrary SQL) — pipe-syntax queries only.
- No per-group/multi-row threshold alerting (e.g. "alert separately per
  host") — a threshold rule's query must resolve to a single row.
- No debounce on the way down — a firing alert resolves on the first
  false evaluation, no symmetric "stay firing for N more minutes" hold.
- No Kubernetes Operator/Helm deployment work — still docker-compose,
  `/deploy` remains stubbed.

## What "done" looks like for Phase 4

**Status: in progress, not shipped.** RBAC enforcement (`api/authz`), the
`alerting`↔`api` service-identity credential, tenant-scoped dashboards,
append-only audit logging, and — since the second pass on this phase —
real per-tenant ClickHouse provisioning and query routing
(`enterprise/internal/tenantprovision`, `enterprise/internal/chrunner`,
wired into a new `enterprise/cmd/enterprise-api` binary alongside plain
`api/cmd/api`) are all built and tested — real integration tests exist
for the ClickHouse pieces, but this environment lost Docker/database
access partway through the phase, so only the audit-logging guarantees
were actually confirmed against a live database; the rest is untested
beyond "compiles, and skips cleanly when no live database is
configured" (see `/docs/phase-4-runbook.md`'s verification-status
section). Human SSO login is now built for both protocols
(`enterprise/internal/loginhandler`: `GET /auth/oidc/login` +
`GET /auth/oidc/callback`, and `GET /auth/saml/login` +
`POST /auth/saml/acs` via `enterprise/internal/saml`'s `crewjam/saml`
wiring, both issuing a real session cookie after resolving tenant/role
from `tenant_memberships`) — genuinely verified, unlike the ClickHouse
pieces, via a real fake IdP for each protocol that performs actual
cryptographic signing and verification (`coreos/go-oidc`'s `oidctest`
for OIDC, `crewjam/saml/samlidp` for SAML — `loginhandler_test.go` and
`saml_test.go`, all passing, including the full login round trip and
negative paths for both), though never tried against a real external IdP
or through a running `enterprise-auth` container. Writing the SAML test
caught and fixed two real bugs in `internal/saml.ParseResponse`: a
missing `r.ParseForm()` call that would have silently broken every real
ACS POST, and email-attribute matching that missed the standard LDAP
"mail" OID IdPs send by default. Tantivy per-tenant index routing is now
built too
(`search/src/registry.rs` + `enterprise/internal/searchclient`) —
**genuinely verified**, like the OIDC login flow: Tantivy is an embedded
library, not a networked service, so the isolation probe (three tenants,
same search term, scoped search returns only that tenant's document)
actually ran in this environment, no Docker needed. That same
Docker-free advantage is what caught a real bug while closing the last
of Phase 4 task 8's four adversarial probes (a mid-provisioning tenant
must be refused, not served): `search/src/registry.rs`'s `IndexRegistry`
opened-or-created an index for any syntactically-valid `tenant_id`,
meaning a query against a tenant that exists in `rbacstore` but isn't
active yet would have silently returned zero results from a
freshly-created empty index instead of being refused --
`chrunner`'s ClickHouse routing had the equivalent guarantee for free
(a mid-provisioning tenant simply isn't in its startup-built connection
map) but Tantivy, a separate process with no Postgres access, had no
way to know. Fixed with a new `enterprise/internal/searchclient.
TenantChecker` (backed by `rbacstore.TenantIsActive`); both halves of
the fix verified Docker-free (`chrunner_test.go`'s and
`searchclient_test.go`'s `TestSearchRefusesMidProvisioningTenant`-shaped
tests) — see `api/queryapi/tenant_isolation_gap_test.go` for the full
accounting of all four probes, now all closed. The deployment-
topology gap that briefly was the largest one is now closed for both
Helm and docker-compose: `deploy/helm/sentry/templates/api.yaml`/
`enterprise-api.yaml` are mutually exclusive on the same
`enterprise.enabled` flag that turns on RBAC/audit/SSO, rendering to the
same Service name/port either way — a Helm-deployed cluster can't
accidentally run the wrong one. `docker-compose.yml`'s `api`/
`enterprise-api` services are now the same mutually-exclusive choice,
gated behind `COMPOSE_PROFILES` (`.env` checks in `single-tenant` as the
zero-config default) and sharing a host port/network-alias trick so
`alerting`/`web` need no conditional logic either way — verified via
`docker compose config` (renders/validates without a daemon, confirms
the two never both appear for one profile selection), not an actual
`docker compose up` in this environment. Per-resource dashboard grants
(the RBAC matrix's "(own/granted)"
qualifier) are now enforced too: `api/dashboards.PermissionStore` (core
interface) implemented by `enterprise/internal/rbacstore.
DashboardPermissions`, wired in only by `enterprise-api` — an Editor can
now only edit/delete a dashboard they created or were granted access to,
not every dashboard in their tenant; managing grants themselves is
stricter still (creator/Admin/Owner only, closing a self-escalation
path). Verified against a fake store (`api/dashboards/handler_test.go`);
real integration tests exist but haven't run against a live Postgres,
same disclosed gap as the rest of this phase's Postgres-backed pieces.
`deploy/operator`'s `Tenant` CRD and `enterprise-api -provision-tenant`
are now unified too, deliberately lightweight rather than making the
K8s controller a second real actor: `-provision-tenant` stays the sole
caller of ClickHouse/`rbacstore`, and (via a new
`enterprise/internal/tenantcrd`, gated on `TENANT_CRD_NAMESPACE`) syncs
its real result into the CRD — a real credential Secret, not the
previous placeholder that authenticated against nothing, and status
fields the reconciler derives `Phase`/`Ready` from instead of
independently guessing "Active" the moment a Tenant object exists. The
tenant-picker's backend protocol is built too: an identity with more
than one `tenant_memberships` row now gets a real `GET
/auth/memberships`/`POST /auth/select-tenant` round trip (a short-lived
pending-login token, distinct from a real session by both Go type and
JWT claim name — a real token-confusion bug this design's own tests
caught before it shipped) instead of the flat refusal Phase 4 shipped
with earlier. Ingest tenant-awareness — the gap this section used to
call "undesigned" — now has a real, if intentionally partial, design:
`ingest` (AGPL core) gained an optional `TenantResolver`
(`ingest/internal/grpcserver`), a per-tenant bearer credential an agent
presents (minted via `enterprise-auth
-create-ingest-credential-tenant=<id>`, validated over the network via a
new `POST /internal/authorize-ingest` endpoint — never an `enterprise/`
import, same boundary shape as `api/authz.Authorizer`), and the
resolved tenant ID is attached to every record as a `tenant_id` Kafka
message header before it's produced. **What's still deferred, clearly**:
nothing downstream reads that header yet — neither `ingest`'s own
ClickHouse writer nor `search`'s independent Redpanda consumer route a
record's write into a per-tenant destination, so every record still
lands in the one shared ClickHouse database/Tantivy index regardless of
which tenant it's now correctly tagged with. That write-routing split
(likely another "second binary," mirroring `enterprise-api`) is real,
scoped, remaining work — attaching a verified tenant identity as early
as possible was deliberately built as a self-contained first step, not
the whole feature. What still keeps this phase from being done: the
actual tenant-picker *page* doesn't exist (`web` has no session/cookie-
handling code at all yet, and `enterprise-auth` has no CORS middleware
for a cross-origin `fetch` with credentials — both real, separately-
scoped frontend gaps), and per-tenant write-routing for ingest per the
above. Full accounting:
`/docs/security/threat-model.md`; step-by-step verification procedure
(not yet run against a live cluster in this environment):
`/docs/phase-4-runbook.md`. The rest of this section describes the exit
bar this phase is aiming at, not a completed state.

Two tenants can be provisioned with SSO (OIDC or SAML), each with their
own users, roles, dashboards, and alert rules, fully isolated at the
ClickHouse/Tantivy connection layer — not by a row filter — with
adversarial integration tests proving no cross-tenant data leakage,
including via the raw-SQL escape hatch and ClickHouse's own `system.*`
tables. A tenant admin can see a query audit trail for their tenant,
backed by append-only storage a compromised application credential
cannot alter (enforced by database grants, not just convention) and
periodically anchored outside the database so tampering is detectable
even against a privileged attacker. See `/docs/phase-4-isolation-design.md`
for the tenant isolation model and why it lives at the connection layer,
`/docs/phase-4-rbac-design.md` for the role/permission model, and
`/docs/security/threat-model.md` for the auth flows and audit-log
integrity guarantees, written for a prospective enterprise customer's
security team.

The tenant-isolation, provisioning, SSO, and RBAC-enforcement mechanisms
live entirely in `enterprise/` (commercial license), confirmed
explicitly rather than assumed: AGPL core (`/api`, `/alerting`, `/web`)
stays genuinely single-tenant, with no multi-tenant mechanism present at
all — `enterprise/` supplies tenant-scoped implementations of core's
already-shipped `querylang/executor.SQLRunner`/`SearchClient` interfaces
rather than core growing tenant awareness. Query-compiler-level "compile
time" enforcement, as originally proposed, turned out not to be
achievable in any module once Phase 2's opaque raw-SQL passthrough is
accounted for — the honest, implemented guarantee is that every code
path (compiled query or raw SQL) is forced through a tenant-scoped
database connection/index that the database's own access control
enforces, not a compiler-injected filter.

Non-goals for this phase (same discipline as every phase so far):
- No deny-override permissions — per-resource grants (e.g. a specific
  user getting edit access to one dashboard) are additive only; a full
  allow/deny ACL system is future work.
- No data retention/deletion policy design for tenant deprovisioning —
  the provisioning state machine includes a `deprovisioning` state, but
  what actually happens to a deprovisioned tenant's data is a separate,
  not-yet-designed compliance question.
- No general multi-cluster orchestration in `/deploy` — scoped to
  proving the per-tenant ClickHouse/Tantivy isolation model works, not a
  fully general multi-cluster system.
- No protection against a privileged ClickHouse/Postgres administrator —
  the isolation and audit-log guarantees in this phase are structural
  defenses against application-layer bugs and injection, not against
  someone with database superuser access; that's an operational control,
  out of scope here and named explicitly, not silently assumed away.

## When in doubt
Ask before: changing the pinned stack, adding a new external dependency
that pulls in a large transitive tree, or making an architectural decision
that isn't already specified in `/docs/architecture.md`.
