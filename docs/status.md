# Cairn OBS — Project Status

Phase-by-phase record of what was built, what "done" meant for each phase,
and how it was verified. Conventions and constraints live in
[`/PROJECT-SPEC.md`](../PROJECT-SPEC.md); the architecture spec is
[`/docs/architecture.md`](architecture.md).

Each phase has a runbook in this directory recording the actual
verification procedure and its results.

## Summary

| Phase | Scope | Status |
|---|---|---|
| 0 | Agent → Redpanda → ingest → ClickHouse, queryable end-to-end | Shipped |
| 1 | Windows Event Log + journald, SQL and full-text paths | Shipped |
| 2 | Unified query language across both stores | Shipped |
| 3 | Dashboards, alert rules, notification delivery | Shipped |
| 4 | RBAC, tenant isolation, audit logging, per-tenant ClickHouse | **In progress** |
| 5 | Frontend redesign and design system | Shipped |
| 6 | License compliance audit and remediation | Shipped |
| 7 | AI-assisted query authoring | Shipped |

**Known verification gaps**, carried forward rather than buried:

- **Phase 4 is not shipped.** Built and unit-tested, but the environment
  lost Docker/database access partway through; only the audit-logging
  guarantees were confirmed against a live database.
- **The Windows agent path has never run on real Windows** — no Windows
  toolchain existed in the build environment. See `/agent/README.md`.
- **Terraform coverage is partial** — see `/terraform/README.md` for the
  full accounting.

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

A single query bar in the web UI and a single `cairnobsctl query` command
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
Helm and docker-compose: `deploy/helm/cairnobs/templates/api.yaml`/
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
`cairnobsctl dashboards permissions list|grant|revoke` is now the CLI
surface for this — `PUT`/`DELETE /dashboards/{id}/permissions/{userId}`
previously had no caller but Go tests and curl.
`deploy/operator`'s `Tenant` CRD and `enterprise-api -provision-tenant`
are now unified too, deliberately lightweight rather than making the
K8s controller a second real actor: `-provision-tenant` stays the sole
caller of ClickHouse/`rbacstore`, and (via a new
`enterprise/internal/tenantcrd`, gated on `TENANT_CRD_NAMESPACE`) syncs
its real result into the CRD — a real credential Secret, not the
previous placeholder that authenticated against nothing, and status
fields the reconciler derives `Phase`/`Ready` from instead of
independently guessing "Active" the moment a Tenant object exists. The
tenant-picker is now fully built, backend and frontend: an identity with
more than one `tenant_memberships` row gets a real `GET
/auth/memberships`/`POST /auth/select-tenant` round trip (a short-lived
pending-login token, distinct from a real session by both Go type and
JWT claim name — a real token-confusion bug this design's own tests
caught before it shipped) instead of the flat refusal Phase 4 shipped
with earlier, and `web/src/routes/select-tenant` is the page that
actually calls it — see the Phase 4 exit-criteria paragraph below for
what changed to make that verifiable in this environment. Ingest
tenant-awareness — the gap this section used to
call "undesigned" — now has a real, if intentionally partial, design:
`ingest` (AGPL core) gained an optional `TenantResolver`
(`ingest/internal/grpcserver`), a per-tenant bearer credential an agent
presents (minted via `enterprise-auth
-create-ingest-credential-tenant=<id>`, validated over the network via a
new `POST /internal/authorize-ingest` endpoint — never an `enterprise/`
import, same boundary shape as `api/authz.Authorizer`), and the
resolved tenant ID is attached to every record as a `tenant_id` Kafka
message header before it's produced. **ClickHouse write-routing is now
built too**: `enterprise/cmd/enterprise-ingest` (another "second binary,"
mirroring `enterprise-api`) reuses `ingest/consumer`'s own flush loop
with `enterprise/internal/chwriter.Registry` swapped in as the writer —
one dedicated ClickHouse connection per tenant, routing each batch's
records by their `tenant_id` tag, fail-closed on an untagged or
unprovisioned tenant. Building it found and fixed a real bug:
`tenantprovision.ProvisionClickHouse` originally granted a tenant's
ClickHouse user `SELECT` only, which would have made every real
per-tenant write fail with a permission error — fixed by granting
`SELECT, INSERT` (one credential, both directions; no cross-tenant
boundary is crossed by also allowing INSERT within a tenant's own
database). **Tantivy write-routing is now built too** —
`search/src/consumer.rs` (a completely independent Redpanda consumer,
not called through `ingest` or `enterprise-ingest` at all: a different
codebase and process) now resolves each record's `tenant_id` header
through the *same* `IndexRegistry` the read side already used, and
routes the write there instead of always into the default index. Unlike
the ClickHouse side, this needed no "second binary": `IndexRegistry`
already lives in this AGPL-core binary (Tantivy has no grant system to
gate a separately-credentialed binary behind, so there was never an
import-boundary reason to split it out -- true regardless of licensing,
though at the time of writing `enterprise/` was still commercially
licensed; both sides are AGPLv3 as of Phase 6), so read and write share one
registry directly. The periodic Tantivy commit now commits every tenant
index that's seen a write, not just the default one
(`IndexRegistry::commit_all`). **The active-tenant gap this same change
originally disclosed is now closed too**: `search/src/tenants.rs`'s
`ActiveTenantTracker` polls a new `GET /internal/active-tenants`
endpoint on `enterprise-auth` (`search` has no Postgres access, unlike
`chwriter.Registry`'s direct `rbacstore` query or the read side's
`searchclient.TenantChecker`, so this needed a network call — the same
"network boundary, not import boundary" shape `ingest`'s
`TenantResolver` already uses against the same service, authenticated
with a RoleService credential the same way `alerting` authenticates to
`api`) and `consumer.rs` refuses any tagged record whose tenant isn't in
the polled allowlist. Off unless both `ENTERPRISE_AUTH_URL` and
`ENTERPRISE_AUTH_SERVICE_TOKEN` are set (same "off unless configured"
default as everything else optional in this codebase); when they are,
startup blocks on the first fetch succeeding and later refresh failures
keep serving the last-known-good set rather than clearing it. Verified
with real HTTP round trips against a hand-rolled TCP test server in this
environment, no live enterprise-auth needed. **This closing move exposed
the ClickHouse side's own gap by comparison** — `chwriter.Registry`'s
writer map was still a startup-only snapshot with no refresh at all, a
real asymmetry once Tantivy's tracker refreshed every minute and
ClickHouse's didn't — so `Registry.StartRefreshing` (new) closes that
too: same one-minute interval, same last-known-good posture on a failed
refresh, opening connections for newly-active tenants and closing ones
no longer active. Both engines now share the same active-tenant
staleness bound instead of one being materially staler than the other.
**The tenant-picker page is now built too**:
`web/src/routes/select-tenant` calls `GET /auth/memberships`/
`POST /auth/select-tenant` via `fetch(..., {credentials: 'include'})`
(new `$lib/api.ts` functions), which needed a second CORS posture
alongside the wildcard-friendly one `enterprise-api` already had —
`api/httpserver.WithCredentialedCORS`, set to a literal origin via a new
`CORS_ALLOWED_ORIGIN` on `enterprise-auth` — since browsers refuse to
honor a wildcard `Access-Control-Allow-Origin` on a credentialed
request. **Genuinely verified in a real browser in this environment**:
a throwaway Node server standing in for `enterprise-auth`'s exact wire
contract (including its plain-text `http.Error` bodies, not JSON) on a
different origin than `web`'s dev server, driven through the full
cross-origin pending-login-cookie round trip, a real click choosing a
tenant, and the post-selection redirect — plus the missing/expired-
pending-login error path — with no Docker or live Postgres/IdP needed,
since the point was exercising `web`'s own fetch/CORS/cookie wiring, not
`enterprise-auth`'s internals (already covered by that package's own
tests). See `/web/README.md`'s "Tenant picker" section for the exact
setup. What's left in this phase now is entirely the caveats already
disclosed above, not an unbuilt feature: the ClickHouse/Postgres-backed
pieces have never run against a real database in this environment, and
nothing here has been tried against a real external IdP or a real
running multi-container deployment. Full accounting:
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
live entirely in `enterprise/` (commercial license at the time this
section was written; relicensed to AGPLv3 in Phase 6, see that phase's
section below), confirmed explicitly rather than assumed: core
(`/api`, `/alerting`, `/web`) stays genuinely single-tenant, with no
multi-tenant mechanism present at all — `enterprise/` supplies
tenant-scoped implementations of core's
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

## What "done" looks like for Phase 5

**Status: shipped.** A ground-up frontend redesign — visual direction,
a real design system, navigation/IA, charting, dashboard panels,
query/search, and alerting UI — plus an accessibility pass, all verified
against a live docker-compose stack with real seeded data, not just
`npm run check`/`npm run build` passing. See `/docs/design-system.md`
for the token system and component library, and
`/docs/phase-5-runbook.md` for the full verification log, including five
real bugs this phase's live-verification discipline caught that a
type-checked, successfully-building frontend would not have surfaced on
its own.

The visual direction ("Signal": near-neutral grayscale UI, color rationed
to the four-tier severity system plus a single interactive accent, real
dark-mode-as-default) was picked from three proposed directions before
any token or component work started, per an explicit stop point in this
phase's brief. The charting library (ECharts, over Observable Plot and
raw D3 — see the design-system doc for the reasoning and the verified
bundle-size/perf numbers) was likewise confirmed before being wired into
every panel type, the second explicit stop point.

Two of the five bugs this phase's verification caught were backend bugs
with no connection to the frontend redesign itself, only surfaced
because getting real dashboard/alert data to verify the new UI against
required actually exercising write paths nothing had exercised since
Phase 4's `tenant_id` migrations landed:

- `alerting`'s `rulestore.Create`/`ApplyTransition` never populated the
  `tenant_id` column Phase 4 added to `alert_state`/`delivery_log` (with
  a `NOT NULL` constraint) — every alert rule created against a
  Phase-4-or-later database silently failed. Existing rows all had a
  value from Phase 4's backfill migration, which is exactly why this
  went uncaught: Phase 4's own verification never created a *new* rule
  post-migration, and its runbook already discloses that Docker access
  was lost partway through that phase.
- `dashboard_panels`'s `viz_type` CHECK constraint was never updated
  alongside `heatmap`'s addition to the Go/TS validators — a three-place
  change (Go validator, TS union, DB constraint), not two.

Both are fixed (`alerting/internal/rulestore/store.go`,
`metadata/migrations/0035_add_heatmap_viz_type.sql`) and confirmed
against a live stack: rule creation → evaluation → firing → a real
(failed, to an intentionally fake webhook) delivery attempt, and a
heatmap panel created, persisted, and rendered end to end. See the
runbook for the other three findings (one more real product bug — a
`findIndex`/nullish-coalescing bug in the chart-pivoting logic that made
every `single_stat` panel render `0` — and two real accessibility
findings caught by axe-core against live-rendered pages with real data,
not fixture data or empty states).

Non-goals for this phase (same discipline as every phase so far):
- No query-language or data-model changes beyond the one narrowly
  justified exception: `heatmap` as a `VizType`, needed to feed a new
  visualization, not a new query capability.
- No changes to tenant isolation, RBAC, SSO, or audit logging — Phase 4's
  surface area is untouched; this phase is presentation-layer only.
- No mobile-phone-width layout — responsive verification stops at
  tablet-landscape width, per the brief's explicit scope ("laptop/
  tablet-landscape," not phone-width).
- No fuzzy search in the command palette, no data-grid virtualization for
  very large result sets, no chart types beyond the five built
  (time-series, bar, single-stat, heatmap, top-N) — real, disclosed
  future work, not oversights.

## What "done" looks like for Phase 6

**Status: shipped.** A full license-compliance audit and remediation
pass across the entire monorepo. Full report:
`/docs/compliance/license-audit-report.md`;
machine-readable inventory: `/docs/compliance/license-inventory.{csv,json}`
(776 rows, 502 unique dependencies across Rust/Go/npm plus Docker base
images and vendored assets); ongoing policy:
`/docs/compliance/license-policy.md`, now enforced in CI
(`.github/workflows/license-compliance.yml` — this repo's first CI
workflow file).

Every dependency was inventoried and classified; 774 of 776 rows
resolved cleanly to AGPLv3-compatible with real citations, not guesses
(see the audit report for the reasoning on each non-obvious case —
dual-licensed crates, MPL-2.0, a license-detector false negative on
`segmentio/asm`); `enterprise/` relicensed to AGPLv3 throughout the
repo, with the deliberate business-model consequence recorded (anyone,
including competitors, can now legally self-host or fork those
features); confirmed no license-gating/entitlement logic ever existed to
remove; a root `LICENSE` file added (there wasn't one before this
phase); CI enforcement wired up and every command verified locally.

**The one real flag — Redpanda's BSL 1.1 license (confirmed against
primary sources for the pinned v24.2.7, not assumed to still be
Apache-2.0) — is resolved, not outstanding**: decision recorded
2026-08-16, accept as-is. Cairn OBS's own use (internal Kafka-protocol
transport, no resale of broker access) sits within BSL's Additional Use
Grant; the harder question — whether a third party self-hosting Cairn OBS
"as a service" using the bundled `docker-compose.yml` could trip BSL's
anti-resale restriction on Redpanda specifically — was judged unlikely
given Cairn OBS's ingest pipeline creates fixed internal topics, not
per-end-user ones, and was accepted as a disclosed, known risk rather
than triggering a swap to Apache Kafka (real resource-footprint cost) or
dropping the bundled broker image (rougher local dev experience). See
the audit report's Redpanda section for the full reasoning, the other
two options that were considered and not chosen, and the condition under
which this should be revisited (an official hosted/managed Cairn OBS
offering, which would make the third-party-SaaS scenario Cairn OBS's own
rather than a hypothetical one).

Non-goals for this phase: replacing permissively-licensed dependencies
with copyleft ones (explicitly out of scope per the phase's own brief);
per-file SPDX license headers across the monorepo's several thousand
source files (a deliberate choice — see the audit report's "Own license
declarations" section for why root `LICENSE` + manifest fields was
judged sufficient); redesigning `favicon.svg` (flagged as a leftover
SvelteKit scaffold asset, not a license blocker — a design task, not a
compliance one).

## What "done" looks like for Phase 7

**Status: shipped.** AI-assisted query authoring: from the same query
bar, a user can (a) get AI-assisted autocomplete, explanations, and fix
suggestions while writing pipe-syntax or SQL queries by hand, and (b)
type a plain-English question and get a generated structured query with
explanation, editable before running — both paths executing through the
unchanged Phase 2 compiler (`api/internal/querylang/planner`,
`api/querylang/executor`) with Phase 4 tenant scoping, cost guardrails,
and audit logging applying identically to both. No cloud dependency
required for the default deployment (self-hosted via Ollama,
`qwen2.5-coder:7b`/`1.5b`, both Apache-2.0 — chosen specifically to keep
Phase 6's license-purity work intact; a pluggable, opt-in, off-by-default
cloud adapter exists for deployments that want one).

Non-negotiable design principle held throughout, confirmed by inspection
of the actual code paths rather than merely asserted: every AI-assisted
or AI-translated query compiles down to and executes through the same
Phase 2 IR and compiler, and passes through the same Phase 4
tenant-scoping enforcement and cost guardrails as a hand-written query —
no parallel execution path, no scoping shortcut, for either track. No AI
code path anywhere constructs a `SQLRunner`, calls `executor.Execute`,
or bypasses `authz.RequireRoleOrService`.

Every AI-assisted suggestion a user explicitly accepts or dismisses
(translate/fix/optimize — deliberately not ghost-text completion or
explain, see the design doc for why) is logged into the same
append-only, hash-chained `audit_log` table Phase 4 built, via a new
`event_type='ai_interaction'` rather than a new table
(`metadata/migrations/0036`) — genuinely verified against a live
Postgres in this environment, not just unit-tested against a fake, the
same rigor Phase 4's own audit-logging guarantees were held to.

Two real product bugs were found and fixed via this phase's live
browser verification — neither would have been caught by
`svelte-check`/`npm run build` — and a real logic bug in the cost/safety
guard itself (an unbounded-aggregation-vs-raw-row distinction) was found
and fixed by the test suite written for it. Full accounting of all
three: `/docs/phase-7-ai-design.md`. Integration tests
(`api/ai/aiapi/integration_test.go`) wire a real `ollama.Client` through
a real `router`/`Handler` against a mock server matching Ollama's actual
wire contract (`hack/mock-ollama`, new — also used for this phase's live
verification), proving the plumbing without needing real model weights;
testing actual model *quality* is deliberately kept out of CI as a
disclosed, periodic human-run checklist item instead — see the design
doc's CI-testability section for the reasoning.

Explicit non-goals for this phase (scoped out, not deferred by oversight):
result summarization, incident narrative generation, and proactive/
unprompted AI suggestions — this phase is query authoring assistance
only (structured and natural-language), not analysis or automation. Real
future-phase candidates, not silently dropped.

See `/docs/phase-7-ai-design.md` for the model-provider architecture,
shared foundation (schema grounding, cost/safety guard), both tracks'
build-and-verification record, and the audit-logging/CI-testability
design; `/docs/phase-7-runbook.md` for the step-by-step live-stack
verification procedure.

