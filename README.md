# Cairn OBS

Open-core, Kubernetes-native log aggregation and observability. Built to match
Splunk on capability while winning on cost-per-GB, with honest multi-tenant
RBAC and a modern language stack.

Licensed **AGPLv3 in its entirety** — including `enterprise/`. See
[Licensing](#licensing).

## What it does

Logs flow from a statically-linked Rust edge agent through Redpanda into a Go
ingest pipeline, landing in ClickHouse for analytics and Tantivy for full-text
search. One query language spans both stores, compiling to a single execution
plan:

```
service=api | where status>=500 | stats count by host | sort -count
message:"connection refused" | stats count by host
```

Raw ClickHouse SQL stays available as an escape hatch and compiles to the same
IR, so performance doesn't depend on which syntax you write.

On top of that sit dashboards, an alerting evaluator with threshold and
absence rules, a CLI (`cairnobsctl`), a Terraform provider, and AI-assisted
query authoring that runs against a self-hosted Ollama model by default — no
cloud dependency.

## Architecture

| Component | Stack |
|---|---|
| Edge agent | Rust, musl static target |
| Transport | Redpanda (Kafka API) |
| Ingest / parse | Go |
| Analytical store | ClickHouse |
| Full-text index | Tantivy (Rust) |
| Control plane / API | Go, gRPC + REST gateway |
| Control-plane metadata | PostgreSQL |
| Frontend | SvelteKit + TypeScript |
| Deployment | Kubernetes Operator (kubebuilder), Helm, docker-compose |

PostgreSQL is scoped strictly to control-plane config — dashboards, panels,
alert rules and state, notification targets, delivery log — because those need
row-level locking and transactional read-modify-write that ClickHouse's
MergeTree family doesn't provide. Log data itself never touches it.

Full spec: [`docs/architecture.md`](docs/architecture.md). Read it before
changing any component; the storage/query split in particular is deliberate.

## Repository layout

Monorepo, one top-level directory per component, each with its own `README.md`,
unit tests, and Dockerfile.

```
agent/      Rust edge agent (Linux + Windows)
transport/  Redpanda topics and schemas
ingest/     Go ingest and parse pipeline
storage/    ClickHouse schema and migrations
search/     Tantivy full-text index service
api/        Control plane, query compiler, RBAC
alerting/   Rule evaluator and notification delivery
metadata/   PostgreSQL schema and migrations
web/        SvelteKit frontend
cli/        cairnobsctl
proto/      gRPC service definitions
terraform/  Terraform provider
enterprise/ SSO, multi-tenancy, per-tenant provisioning
deploy/     Helm charts and Kubernetes operator
docs/       Architecture, design docs, per-phase runbooks
hack/       Development scripts
```

`enterprise/` stays a separate module that core never imports from. Since the
Phase 6 relicensing that boundary is architectural rather than legal — it keeps
core buildable and deployable standalone, and keeps tenant resolution
server-side.

## Running locally

```sh
docker compose up
```

The web UI comes up on <http://localhost:3000> and the API on `:8080`;
alerting is on `:8081` and enterprise auth on `:8082`. The agent connects to
ingest over mTLS gRPC on `:4317`. `search` is reachable only on the compose
network — it publishes no host port.

`COMPOSE_PROFILES` in `.env` selects the query-serving binary — `single-tenant`
(default) or `enterprise` for the multi-tenant path. They're mutually
exclusive, the same choice Helm's `enterprise.enabled` flag makes for a real
cluster. Override per invocation:

```sh
COMPOSE_PROFILES=enterprise docker compose up
```

Kubernetes deployment via the Helm chart in [`deploy/`](deploy/README.md).

## Status

Built in phases; each has a runbook in `docs/` recording how it was verified.
Full per-phase detail is in [`docs/status.md`](docs/status.md).

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

**Phase 4 is not shipped.** The code is built and tested, but the environment
lost Docker and database access partway through, so only the audit-logging
guarantees were confirmed against a live database. The rest compiles and skips
cleanly when no live database is configured, but is otherwise unverified — see
the verification-status section of
[`docs/phase-4-runbook.md`](docs/phase-4-runbook.md).

The Windows agent code (`EvtSubscribe`, ETW, service registration) has never
run on real Windows — no Windows toolchain existed in the build environment.
ETW additionally sits behind a feature flag, since it needs elevated
privileges. Details in [`agent/README.md`](agent/README.md).

Terraform provider coverage is partial by necessity: dashboards and panels have
full CRUD, while alert rules and notification targets are create/destroy only,
because `alerting` exposes no `PUT /rules/{id}` or `PUT /targets/{id}` to
update against. Tenant and RBAC resources are disclosed future work —
[`terraform/README.md`](terraform/README.md) accounts for exactly what exists.

## Contributing

- Conventional commits. Every change should be a logically complete,
  independently revertible unit.
- Rust: `cargo clippy --all-targets -- -D warnings` must pass.
- Go: `go vet` and `golangci-lint`, no globals for shared state.
- Every UI action must map to a documented REST/gRPC call — no UI-only logic.
  The CLI and Terraform provider are first-class, not afterthoughts.
- Prefer boring, well-understood dependencies. This is infrastructure software;
  operators need to trust it.

## Licensing

AGPLv3, no exceptions — see [`LICENSE`](LICENSE). `enterprise/` was under a
commercial-license stub from Phase 4 through Phase 5; Phase 6 relicensed it to
match core. The full record and its business-model consequences are in
[`docs/compliance/license-audit-report.md`](docs/compliance/license-audit-report.md).

The default AI deployment uses `qwen2.5-coder` (Apache-2.0) via Ollama,
chosen specifically to keep that license purity intact. The cloud adapter is
pluggable, opt-in, and off by default.
