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

A user can build a multi-panel dashboard from saved Phase 2 queries (at
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
  tenant/org assumed. New tables carry a `tenant_id` column so Phase 4's
  retrofit doesn't require a schema migration + backfill, but nothing
  reads or enforces it yet.
- No raw-SQL dashboard panels (time-range injection isn't reliable
  against arbitrary SQL) — pipe-syntax queries only.
- No per-group/multi-row threshold alerting (e.g. "alert separately per
  host") — a threshold rule's query must resolve to a single row.
- No debounce on the way down — a firing alert resolves on the first
  false evaluation, no symmetric "stay firing for N more minutes" hold.
- No Kubernetes Operator/Helm deployment work — still docker-compose,
  `/deploy` remains stubbed.

## When in doubt
Ask before: changing the pinned stack, adding a new external dependency
that pulls in a large transitive tree, or making an architectural decision
that isn't already specified in `/docs/architecture.md`.
