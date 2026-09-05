# Project: Cairn OBS — Distributed Log Aggregation & Observability Platform

## Mission
Build a self-hosted, Kubernetes-native centralized logging platform that
rivals Splunk on features but wins on cost-per-GB and a modern language
stack. Full architecture spec is in `/docs/architecture.md` — read
it before touching any component. Do not deviate from the storage/query split
described there without flagging it to me first.

Cairn OBS is positioned against **Cribl** as well, which is a different claim
rather than the same one twice: Splunk is the destination and Cairn OBS
replaces it; Cribl is the road, and Cairn OBS is currently a road that exposes
none of a pipeline's controls. Reconciling the two — including the part where
cheap storage removes the usual reason to buy Cribl at all, and the part where
competing with it means helping data leave this platform — is
[`/docs/positioning.md`](docs/positioning.md), along with the four phases of
processing, routing, archive/replay and fleet work it implies. Read it before
proposing anything pipeline-shaped.

## Non-negotiable constraints
- Distro-agnostic Linux agent: must run identically on RHEL/Debian/Arch/SUSE
  derivatives via a statically-linked musl binary. No glibc runtime deps.
- Windows support via native ETW/Event Log API, not a WSL shim.
- **AGPLv3 for the entire project, no exceptions.** The `enterprise/`
  module (SSO/multi-tenancy/compliance) was under a commercial-license
  stub from Phase 4 through Phase 5; Phase 6 relicensed it to AGPLv3,
  matching core — see `/docs/compliance/license-audit-report.md` for the
  full record and its business-model consequences. `enterprise/` stays a
  separate directory that core never imports from, but that boundary is
  now architectural only (keeps core buildable/deployable standalone,
  keeps tenant resolution server-side), not a licensing wall.
- Schema-on-write with OTel semantic conventions as the default schema, with
  schema-on-read fallback for unstructured text.
- Every UI action must correspond to a documented REST/gRPC call. No
  UI-only logic. CLI (`cairnobsctl`) and Terraform provider are first-class,
  not afterthoughts. **Status**: `cairnobsctl` has been built out phase by
  phase since Phase 3. The Terraform provider (`/terraform`) only exists
  as of this note -- four resources (`cairnobs_dashboard` and
  `cairnobs_dashboard_panel`, both full CRUD, panels as their own resource
  rather than a nested block since the API manages them independently
  of their parent dashboard; `cairnobs_alert_rule` and
  `cairnobs_notification_target`, both create/destroy only -- `alerting`
  has no `PUT /rules/{id}` or `PUT /targets/{id}` to update against),
  each paired with a read-only data source, built on HashiCorp's
  `terraform-plugin-framework`, reusing the exact same REST contracts
  `cairnobsctl dashboards apply`/web's dashboard export and
  `cairnobsctl alerts apply` already use. Tenant/RBAC resources are real,
  disclosed future work -- see
  `/terraform/README.md` for the full accounting of what is and isn't
  built, and the same
  "written but not run against a live stack" verification caveat as
  everything else Docker-gated in this repo.

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

## Project status
Phase-by-phase scope, what "done" meant for each, and the verification
record — including what is *not* yet shipped and what remains unverified —
live in [`/docs/status.md`](docs/status.md). Read it before assuming a
capability works end-to-end; several are built but unconfirmed against a
live stack, and the per-phase runbooks in `/docs` record exactly how each
was checked.

## When in doubt
Ask before: changing the pinned stack, adding a new external dependency
that pulls in a large transitive tree, or making an architectural decision
that isn't already specified in `/docs/architecture.md`.
