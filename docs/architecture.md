# Sentry Architecture

> **Status:** Updated through Phase 4. The component map/diagram below is
> still the Phase 0 request path (agent → ingest → ClickHouse → api →
> web) — it was never redrawn for the full-text search, dashboards/
> alerting, or enterprise/ additions; see each phase's runbook
> (`/docs/phase-N-runbook.md`) for what was actually verified when it
> shipped. The component responsibilities table and the sections below
> the diagram are kept current. Phase 0's original framing ("draft,
> correct as needed") still applies to anything not yet built.

## Mission

Open-core, Kubernetes-native centralized logging platform. Compete with
Splunk on features; win on cost-per-GB, a modern language stack, and
multi-tenant RBAC that's actually honest about its guarantees.

## Component map

```
┌──────────┐   gRPC/mTLS   ┌──────────┐   produce   ┌───────────┐  consume  ┌────────────┐
│  agent   │ ────────────▶ │  ingest  │ ──────────▶ │ Redpanda  │ ────────▶ │  ingest     │
│ (Rust)   │                │ (Go)     │             │ (Kafka API)│           │  consumer   │
└──────────┘                └──────────┘             └───────────┘           │  (Go)       │
                                                                              └──────┬─────┘
                                                                                     │ batch INSERT
                                                                                     ▼
                                                                              ┌────────────┐
                                                                              │ ClickHouse │
                                                                              └─────┬──────┘
                                                                                    │ SQL
                                                                        ┌───────────▼───────────┐
                                                                        │ api (Go: gRPC+REST)    │
                                                                        └───────────┬───────────┘
                                                                                    │ REST
                                                                        ┌───────────▼───────────┐
                                                                        │ web (SvelteKit)        │
                                                                        └────────────────────────┘
```

Decision (confirmed 2026-08-12): Redpanda stays in the Phase 0 path. The
`ingest` service's gRPC front end produces to Redpanda rather than writing
ClickHouse directly; a separate consumer path reads from Redpanda and batches
inserts into ClickHouse. This exercises the real transport layer from day
one instead of deferring it, and keeps Kafka credentials off the edge agent.

## Storage / query split

- **ClickHouse** is the analytical store of record for structured log data:
  timestamp, host, service, severity, message, plus a `Map(String,String)`
  for arbitrary structured fields. Partitioned by day, ordered by
  `(service, timestamp)`.
- **Tantivy** (Phase 1) will provide full-text indexing over the `message`
  field and unstructured payloads, queried out-of-band from ClickHouse and
  joined by a log identifier. Not built in Phase 0.
- **Schema-on-write** using OTel semantic conventions as the default log
  schema; schema-on-read fallback for unstructured/raw text that doesn't fit
  the structured columns (captured via the `Map` column and/or a raw
  passthrough field).
- **Postgres** (Phase 3) holds control-plane config only — dashboards,
  panels, notification targets, alert rules/state, delivery log, and
  (Phase 4) tenants/users/tenant_memberships/audit_log. Never log data;
  ClickHouse/Tantivy stay the only place a log record itself lives. See
  `/docs/phase-3-dashboard-design.md` for why ClickHouse's MergeTree
  family isn't a fit for this (no real row-level locking/transactional
  read-modify-write).

This split is not to be changed without discussion — see CLAUDE.md.

## Component responsibilities

| Component | Responsibility |
|---|---|
| `agent` (Rust, musl) | Tail a log file or read journald; parse RFC 5424 syslog with raw passthrough fallback; batch; ship via gRPC/mTLS to `ingest`. Windows (ETW/Event Log) code exists but is unverified on real Windows hardware — see `/agent/README.md`. |
| `proto` | Shared `.proto` contracts for agent↔ingest and api↔search gRPC, versioned independently of either side. |
| `transport` | Redpanda docker-compose + topic provisioning scripts. No application code. |
| `ingest` (Go) | gRPC server accepting agent connections; produces normalized OTel-log-like records to Redpanda; separate consumer reads from Redpanda and batch-writes to ClickHouse. No tenant concept — every record lands in the one shared `logs` table regardless of source (see "Tenant isolation" below). |
| `storage` | ClickHouse schema migrations + docker-compose for local/homelab. |
| `search` (Rust, Phase 1) | Consumes the same Redpanda topic `ingest` does (own offset tracking), builds a Tantivy full-text index over `message`, serves matches over gRPC. Writes always go to one shared (default) index (`ingest` isn't tenant-aware); reads can be scoped per-tenant via `SearchRequest.tenant_id` and `src/registry.rs`'s `IndexRegistry` (Phase 4) — see "Tenant isolation" below. |
| `api` (Go) | gRPC + REST gateway. `POST /query` compiles pipe-syntax or raw SQL to one IR, executed across ClickHouse/Tantivy (`/docs/query-language-design.md`). `internal/dashboards` is CRUD only — panel query execution happens client-side, reusing `/query`. `internal/authz` (Phase 4) enforces RBAC via a network call to `enterprise-auth`, never an import. |
| `alerting` (Go, Phase 3) | Evaluates alert rules on an interval, calls `api`'s `POST /query` (via a `RoleService` credential once Phase 4 auth is configured — see `/docs/phase-4-isolation-design.md`'s alerting↔api gap), delivers firing/resolved notifications (webhook/Slack/PagerDuty). |
| `enterprise` (Go, commercial license, Phase 4) | OIDC login (`internal/loginhandler`'s `/auth/oidc/login`+`/auth/oidc/callback`) and SAML login (`/auth/saml/login`+`/auth/saml/acs`, via `internal/saml`'s `crewjam/saml` wiring) — both a real IdP round trip, each verified with a real fake IdP (`coreos/go-oidc`'s `oidctest`, `crewjam/saml`'s `samlidp`) but not a real external one, RBAC storage (`internal/rbacstore`), session/service-token issuance (`internal/session`), the append-only audit log (`internal/audit`), `enterprise-auth`'s HTTP surface (`/internal/authorize`, `/auth/features`), per-tenant ClickHouse provisioning (`internal/tenantprovision`) and query routing (`internal/chrunner`), and `cmd/enterprise-api` — a second binary combining core's `api/queryapi`/`api/dashboards` handlers with these tenant-aware implementations. Never imported by core — see "Licensing boundary" below. Also `internal/searchclient` (per-tenant Tantivy routing, wired the same way into `search`). |
| `web` (SvelteKit, static build) | Query bar, dashboards, alerts, and (Phase 4) a settings page that renders SSO status via a runtime capability check (`GET /auth/features`) rather than bundling enterprise-licensed components. |
| `cli` (`sentryctl`) | `ping`, `query`, `dashboards` (list/get/apply), `alerts` (list/get/apply). `$SENTRYCTL_TOKEN`, if set, is forwarded as a Bearer credential (Phase 4). |
| `deploy` | A Helm chart covering every `docker-compose.yml` service, plus (Phase 4) a small Go Operator managing one CRD (`Tenant`) that provisions a per-tenant ClickHouse credential Secret. Never applied to a live cluster in the environment this was built in — see `/deploy/README.md`'s verification section before trusting it. |

## Tenant isolation model (Phase 4)

Full design rationale: `/docs/phase-4-isolation-design.md`. Full honest
accounting of what's actually enforced vs. designed-only:
`/docs/security/threat-model.md` — read that before assuming any claim
below holds for log data specifically.

**As designed:** one dedicated ClickHouse database + narrowly-granted
user per tenant (never the shared `default`/admin credential), one
dedicated Tantivy index directory per tenant, `system.*` access revoked
per tenant, connections resolved from an immutable per-tenant map (never
a shared pool with session-level `USE`). Isolation lives at the
**connection layer** — every query, compiled or raw SQL, is forced
through a tenant-scoped connection the database's own access control
enforces — not at the query-compiler layer, since Phase 2's raw-SQL
escape hatch is opaque to any compiler-injected filter.

**As built, currently:**

- Role-based access control (`api/authz`) is live on `/query`
  and `/dashboards`, resolved via `enterprise-auth` over HTTP.
- Control-plane tenant scoping is live for dashboards
  (`api/dashboards`'s store filters every query by the
  authenticated identity's tenant, never a client-supplied field).
- The `alerting`↔`api` service-identity gap (task 2's finding) is
  closed: a `RoleService` credential, distinct from every human role.
- **ClickHouse connection-layer isolation is built**, but lives in a
  second binary: `enterprise/internal/tenantprovision` (real `CREATE
  DATABASE`/`CREATE USER`/`GRANT` against ClickHouse) and
  `enterprise/internal/chrunner` (a per-tenant connection registry
  implementing `api/querylang/executor.SQLRunner`, resolving the
  right tenant's connection from the authenticated identity in request
  context) are wired into `enterprise/cmd/enterprise-api` — a binary
  that imports both `api`'s handler packages and enterprise's
  tenant-aware implementations (the allowed `enterprise → api` import
  direction; core still never imports `enterprise/`). Real integration
  tests assert a tenant cannot read another tenant's database by
  fully-qualified name, and that `system.query_log`/`system.tables`/
  `SHOW DATABASES` don't leak across tenants either — written but not
  yet run against a live ClickHouse in this environment, see
  `/docs/security/threat-model.md` and `/docs/phase-4-runbook.md`'s
  verification-status sections. Plain `api/cmd/api` still exists,
  unchanged, with its single shared connection — nothing forces a
  deployment to run `enterprise-api` instead, and nothing flags it if it
  doesn't.
- **Tantivy index-layer isolation is built, and verified.**
  `search/src/registry.rs`'s `IndexRegistry` resolves
  `SearchRequest.tenant_id` (added to `proto/sentry/search/v1/
  search.proto`) to its own on-disk index, opened on demand;
  `enterprise/internal/searchclient` sets that field from the
  authenticated request identity, the same "read from ctx, fail closed"
  shape `chrunner` uses. Unlike the ClickHouse pieces, this one actually
  ran in the environment it was built in — Tantivy is an embedded
  library, so the cross-tenant isolation probe needed no live database
  or Docker to execute for real, and it passed.
- **Ingest identity, ClickHouse write-routing, and Tantivy write-routing
  are all now built.** `ingest` (AGPL core) gained an optional `TenantResolver`
  (`ingest/internal/grpcserver`): an agent presents a per-tenant bearer
  credential (`enterprise-auth -create-ingest-credential-tenant=<id>`
  mints one, only its hash stored), validated over the network via a new
  `POST /internal/authorize-ingest` endpoint (never an `enterprise/`
  import — same "network boundary, not import boundary" shape
  `api/authz.Authorizer` already uses), and the resolved tenant ID rides
  as a `tenant_id` Kafka message header on every record produced.
  `enterprise/cmd/enterprise-ingest` (another "second binary," mirroring
  `enterprise-api`) consumes that tag: it reuses `ingest/consumer`'s own
  flush loop with `enterprise/internal/chwriter.Registry` swapped in as
  the writer, routing each batch's records to a per-tenant ClickHouse
  connection (one per tenant, built from `rbacstore.
  ListProvisionedDataSources` — the same source `chrunner` already uses
  for reads), fail-closed on an untagged or unprovisioned tenant.
  Building it found and fixed a real bug: `tenantprovision.
  ProvisionClickHouse` originally granted a tenant's ClickHouse user
  `SELECT` only, which would have made every real per-tenant write fail
  with a permission error — fixed by granting `SELECT, INSERT` (one
  credential, both directions; no cross-tenant boundary is crossed by
  also allowing INSERT within a tenant's own database). `search`'s
  independent Redpanda consumer (a different codebase — Rust — and a
  different process, not reachable through either `ingest` or
  `enterprise-ingest`) is now write-routed too:
  `search/src/consumer.rs` resolves each record's `tenant_id` header
  through the *same* `IndexRegistry` the read side
  (`search/src/registry.rs` + `enterprise/internal/searchclient`)
  already used, and writes there instead of always into the default
  index. No "second binary" needed here, unlike ClickHouse — Tantivy has
  no grant system to gate a commercially-licensed credential behind, so
  `IndexRegistry` already lived directly in this AGPL-core binary, and
  read/write just share it. One gap is disclosed rather than closed by
  this change: unlike `chwriter.Registry` (an active-tenants-only
  snapshot built at startup) and unlike the read side (gated by
  `searchclient.TenantChecker`), this consumer's `resolve()` call has no
  active-tenant check — the process has no Postgres access to check
  against — so a still-valid-but-should-be-revoked ingest credential can
  cause an index directory to be created for a tenant that's no longer
  active. Narrow blast radius (an orphan, isolated, empty index, not
  cross-tenant leakage, and only reachable with a real signed
  credential), but real; see `search/src/registry.rs`'s doc comment on
  `resolve`.
- `deploy/operator`'s `Tenant` CRD and `enterprise-api -provision-tenant`
  are now unified, deliberately lightweight: `-provision-tenant` stays
  the sole real actor (ClickHouse + `rbacstore`), and now also syncs its
  real result into the `Tenant` CRD (`enterprise/internal/tenantcrd`) --
  a real credential Secret, and status fields the reconciler
  (`deploy/operator/internal/controller`) derives `Phase`/`Ready` from
  rather than independently guessing. The reconciler itself gained no
  new credentials and still never touches ClickHouse/Postgres.

**The deployment-topology gap is closed for both Helm and
docker-compose**: `deploy/helm/sentry/templates/api.yaml`/
`enterprise-api.yaml` are mutually exclusive on `enterprise.enabled`,
rendering to the same Service name and port either way, so a
Helm-deployed cluster can't accidentally run the wrong binary — the same
flag that turns on RBAC/audit/SSO now also chooses the query binary.
`docker-compose.yml`'s `api`/`enterprise-api` services are the same
mutually-exclusive choice via `COMPOSE_PROFILES` (`.env` defaults to
plain `api`), sharing a host-port/network-alias trick so `alerting`/
`web` need no conditional config either way. With both storage engines'
connection/index-layer mechanisms built, deployment topology enforced at
both the Helm and docker-compose layers, the two provisioning
mechanisms unified, both storage engines' write paths per-tenant-routed,
and the tenant-picker frontend page now built and browser-verified
(`web/src/routes/select-tenant`, `api/httpserver.WithCredentialedCORS`
— see `/CLAUDE.md`'s Phase 4 section and `/web/README.md`'s "Tenant
picker" section), the remaining gaps in this phase are entirely the
already-disclosed live-verification caveats: the ClickHouse/Postgres-
backed pieces have never run against a real database in this
environment, and nothing here has been tried against a real external
IdP or a real running multi-container deployment.

## Licensing boundary

AGPLv3 for core + agents. Enterprise features (SSO, RBAC storage, audit
logging) live under `enterprise/` (commercial license stub, added
Phase 4). AGPL code must never import from `enterprise/` — enforced in
CI by `hack/check-tenant-boundary.sh`, which greps every build for the
import edge. Where core needs a decision only `enterprise/` can make
(is this request authorized, what SSO is configured), it calls
`enterprise-auth` over plain HTTP instead
(`api/authz.HTTPAuthorizer`, `web`'s `GET /auth/features`) —
the same "network boundary, not import boundary" shape `/alerting`↔`api`
already used before `enterprise/` existed.

## Non-negotiables carried from CLAUDE.md

- Rust agent: statically linked musl, `x86_64-unknown-linux-musl` and
  `aarch64-unknown-linux-musl`, no glibc runtime deps.
- Windows support via native ETW/Event Log API, not WSL — designed
  (Phase 1) but still unverified on real Windows hardware.
- Every UI action maps to a documented REST/gRPC call — no UI-only logic.
- Pinned stack (see CLAUDE.md table) — no substitutions without discussion.

## Explicitly out of scope (current, Phase 4)

Per `/CLAUDE.md`'s Phase 4 non-goals and `/docs/security/threat-model.md`:
deny-override permission grants, a data retention/deletion policy for
deprovisioned tenants, general multi-cluster orchestration in `/deploy`,
and any defense against a privileged ClickHouse/Postgres administrator —
every isolation and audit-integrity guarantee here is a structural
defense against application-layer bugs, not an operational control.

## Open questions for you to resolve

- Retention/TTL policy for the ClickHouse `logs` table — not specified yet,
  deferred until storage sizing is a real concern.
- Exact OTel log schema field mapping (which OTel resource/log attributes
  map to which ClickHouse columns) — Phase 0 uses a minimal subset
  (timestamp, host, service, severity, message, attributes map); full
  mapping deferred.
- mTLS certificate provisioning/rotation story for agents — Phase 0 will use
  a static dev CA and manually issued certs; production PKI design is
  out of scope here.
