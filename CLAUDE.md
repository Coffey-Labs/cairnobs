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

A Windows Event Log entry and a Linux journald entry should both be
queryable via SQL (the ClickHouse path) and via free-text search (the
Tantivy path), from the same UI, within a few seconds of being generated.

Non-goals for this phase (same "resist scope creep" discipline as Phase 0):
no alerting, no dashboards, no SPL-like query layer, no multi-tenancy, and
no unified query experience — two separate boxes on two separate pages is
correct for Phase 1; unifying them is Phase 2's job.

ETW and WEF (Windows Event Forwarding) are *designed* in this phase but not
required to be running for "done": ETW ships behind a feature flag most
environments won't enable (it needs elevated privileges), and WEF's
receiver-side is explicitly deferred rather than built now — see
`/docs/phase-1-runbook.md` for both. Only the Event Log source needs to
actually be running end-to-end for this phase to count as done.

## When in doubt
Ask before: changing the pinned stack, adding a new external dependency
that pulls in a large transitive tree, or making an architectural decision
that isn't already specified in `/docs/architecture.md`.
