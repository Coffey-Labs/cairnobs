# Sentry Architecture

> **Status:** Draft, Phase 0 scope. Written from the project constraints and
> task list at kickoff, not transcribed from a pre-existing spec. Treat as a
> starting point to correct, not a settled design — flag anything that
> doesn't match your intent before implementation leans on it further.

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

This split is not to be changed without discussion — see CLAUDE.md.

## Component responsibilities (Phase 0)

| Component | Responsibility |
|---|---|
| `agent` (Rust, musl) | Tail a log file or read journald; parse RFC 5424 syslog with raw passthrough fallback; batch; ship via gRPC/mTLS to `ingest`. |
| `proto` | Shared `.proto` contracts for the agent↔ingest gRPC service, versioned independently of either component. |
| `transport` | Redpanda docker-compose + topic provisioning scripts. No application code. |
| `ingest` (Go) | gRPC server accepting agent connections; produces normalized OTel-log-like records to Redpanda; separate consumer reads from Redpanda and batch-writes to ClickHouse. |
| `storage` | ClickHouse schema migrations + docker-compose for local/homelab. |
| `api` (Go) | gRPC + REST gateway. Phase 0: one crude `POST /query` endpoint, SELECT-only, proxying to ClickHouse. Real SPL-like query layer is Phase 2. |
| `web` (SvelteKit) | Single page: SQL text box, submit, results table. No auth, no styling polish. |
| `cli` (`sentryctl`) | Stub. Single `ping` command for now. |
| `deploy` | Helm charts, k8s manifests. Stubbed in Phase 0; docker-compose is the real local/dev path. |

## Licensing boundary

AGPLv3 for core + agents. Enterprise features (SSO, multi-tenancy,
compliance) live under `enterprise/` (not yet created — out of scope for
Phase 0) under a commercial license stub. AGPL code must never import from
`enterprise/`. No enterprise-gated code exists yet in this repo; this
section documents the boundary so nothing added later crosses it by
accident.

## Non-negotiables carried from CLAUDE.md

- Rust agent: statically linked musl, `x86_64-unknown-linux-musl` and
  `aarch64-unknown-linux-musl`, no glibc runtime deps.
- Windows support (Phase 1+) via native ETW/Event Log API, not WSL.
- Every UI action maps to a documented REST/gRPC call — no UI-only logic.
- Pinned stack (see CLAUDE.md table) — no substitutions without discussion.

## Explicitly out of scope for Phase 0

Windows agent, alerting, dashboards, multi-tenancy, Tantivy full-text
search, the real SPL-like query language, enterprise module code.

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
