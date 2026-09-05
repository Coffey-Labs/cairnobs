<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)"
            srcset="web/src/lib/assets/logo-horizontal-dark.svg">
    <img src="web/src/lib/assets/logo-horizontal-light.svg"
         alt="Cairn OBS" width="420">
  </picture>
</p>

<p align="center">
  Open-core, Kubernetes-native log aggregation and observability.<br>
  Built to match Splunk on capability while winning on cost-per-GB,<br>
  with honest multi-tenant RBAC and a modern language stack.<br>
  Positioned against Cribl too — see <a href="docs/positioning.md">positioning</a>
  for why that is a different claim, and what it means we still have to build.
</p>

<p align="center">
  Licensed <strong>AGPLv3 in its entirety</strong> — including
  <code>enterprise/</code>. See <a href="#licensing">Licensing</a>.
</p>

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

### Signing in

A plain `docker compose up` has **no authentication** — every runbook in
`docs/` verifies the pipeline with bare `curl` against `/query`, and those
steps depend on that. For a login screen without an identity provider behind
it, add the local-login overlay:

```sh
docker compose -f docker-compose.yml -f docker-compose.local-auth.yml up -d --build
docker compose -f docker-compose.yml -f docker-compose.local-auth.yml run --rm api -seed-admin
```

The second command prints a generated password once. Full detail, including
what each setting does and how each one fails on its own, is in
[`docs/local-login.md`](docs/local-login.md).

SSO (OIDC/SAML) is the other option and takes precedence over local login
where both are configured — see [`docs/phase-4-runbook.md`](docs/phase-4-runbook.md).

Kubernetes deployment via the Helm chart in [`deploy/`](deploy/README.md).

## Status

**Read this before the table.** Cairn OBS is pre-1.0 and has not run a
production workload. What it has done is get built in phases, with each phase
verified against real infrastructure and a runbook in `docs/` recording
exactly how — including what the verification found, and what it could not
reach.

That last part is why the caveats below this table are unusually long. They
are disclosed, not discovered: nothing here is called *shipped* on the
strength of passing tests alone, and anything that has only been proven in one
environment, against one vendor, or not at all says so by name. A shorter
Status section would not mean a more finished product, only a less careful
one. If you are evaluating this, the honest summary is that the capability is
real and the operational mileage is not there yet — every phase has run
somewhere, none of it has run anywhere for a year under load.

Full per-phase detail, including the verification record for each, is in
[`docs/status.md`](docs/status.md). Where the project is going, and why it is
positioned against both Splunk and Cribl, is in
[`docs/positioning.md`](docs/positioning.md).

| Phase | Scope | Status |
|---|---|---|
| 0 | Agent → Redpanda → ingest → ClickHouse, queryable end-to-end | Shipped |
| 1 | Windows Event Log + journald, SQL and full-text paths | Shipped |
| 2 | Unified query language across both stores | Shipped |
| 3 | Dashboards, alert rules, notification delivery | Shipped |
| 4 | RBAC, tenant isolation, audit logging, per-tenant ClickHouse | Shipped |
| 5 | Frontend redesign and design system | Shipped |
| 6 | License compliance audit and remediation | Shipped |
| 7 | AI-assisted query authoring | Shipped |

**Phase 4 is shipped, and the environment that proved it is gone.** Every
control the phase defines was verified against real infrastructure at least
once — a docker-compose stack with real ClickHouse and Postgres, a local `kind`
cluster, and both SSO protocols against a real Auth0 tenant — finding eight
bugs that no amount of Docker-free testing could have caught. The prototype
VPS was retired on 2026-09-04, so that verification is a record rather than
something you can re-run: see
[`docs/phase-4-runbook.md`](docs/phase-4-runbook.md).

Two limits worth stating plainly. SSO has been tried against one IdP, not two,
and no production-grade cluster has run this. And `demo.cairnobs.org` is **not**
evidence for any of it — the demo runs the single-tenant profile, so it
exercises the OSS path and says nothing about RBAC or tenant isolation.

The Windows agent code (`EvtSubscribe`, ETW, service registration) has never
run on real Windows — no Windows toolchain existed in the build environment.
ETW additionally sits behind a feature flag, since it needs elevated
privileges. Details in [`agent/README.md`](agent/README.md).

Terraform provider coverage is partial by necessity: dashboards and panels have
full CRUD, while alert rules and notification targets are create/destroy only,
because `alerting` exposes no `PUT /rules/{id}` or `PUT /targets/{id}` to
update against. Tenant and RBAC resources are disclosed future work —
[`terraform/README.md`](terraform/README.md) accounts for exactly what exists.

### What would close the gap to production-ready

Named here so the list above reads as a plan rather than an apology, and so
anyone evaluating this knows what they would be waiting for:

1. **A second identity provider.** SSO works against Auth0 for both OIDC and
   SAML; one vendor is an implementation, two is a standard.
2. **A real cluster.** The Helm chart has been installed against a local
   `kind` cluster, which proves the manifests and nothing about scheduling,
   storage classes or node failure.
3. **The Windows agent on Windows.** The code is written and reviewed; no
   Windows toolchain has ever compiled it, let alone run it.
4. **Sustained load.** Every phase was verified functionally. Nothing here has
   been run at volume for long enough to find the failures that only show up
   after a week.
5. **Somebody else's data.** Every deployment so far has been ours.

None of that is research; it is time on real infrastructure. It is also
exactly the list a pilot deployment would work through, which is the honest
next step for this project rather than a 1.0 tag.

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

Copyright (C) 2026 Coffey Labs.

AGPLv3, no exceptions — see [`LICENSE`](LICENSE). `enterprise/` was under a
commercial-license stub from Phase 4 through Phase 5; Phase 6 relicensed it to
match core. The full record and its business-model consequences are in
[`docs/compliance/license-audit-report.md`](docs/compliance/license-audit-report.md).

The default AI deployment uses `qwen2.5-coder` (Apache-2.0) via Ollama,
chosen specifically to keep that license purity intact. The cloud adapter is
pluggable, opt-in, and off by default.
