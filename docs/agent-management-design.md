# Agent inventory, management, and remote config

Extends `/docs/agent-heartbeat-monitoring.md` (heartbeat + absence
alerting). This adds three things to the web UI: an inventory view of
every agent that's checked in, read-only visibility into each agent's
actual running config, and the ability to remotely edit a narrow,
deliberately-scoped subset of that config.

## Scope decision (confirmed before building)

"Agent management" could mean several different things with very
different risk profiles. Confirmed up front: this covers inventory,
config visibility, and remote config *editing* — explicitly **not**
remote lifecycle commands (restart/stop/uninstall). That's a real
command-and-control channel across every managed host and deserves its
own security design (strict RBAC, full audit trail) before it's built,
not something to fold in as a side effect of a config-editing feature.

**Update — restart is now built** (punch-list item 1, see the
"Lifecycle commands" section below); stop and uninstall remain
deliberately out of scope, for reasons specific to each. Building
restart confirmed the "strict RBAC, full audit trail" precondition
mattered in practice, not just as a stated principle: it's gated at
`RoleAdmin` (stricter than config editing's `RoleEditor`) and logged
into the same `audit_log` table Phase 7's AI interactions use.

## Why this is still pull, not push, on the wire

The agent's transport is unchanged and still push-only: it dials
*out* to `ingest` over mTLS; nothing in the platform ever reaches into
an agent. See `/docs/agent-heartbeat-monitoring.md`'s "Design: why this
is a heartbeat, not a true pull" section for the full reasoning (NAT/
dynamic-IP tolerance, no inbound port on any remote host). Remote config
editing extends that same posture: an operator's edit doesn't get
pushed to the agent at the moment it's saved. It sits in Postgres until
the agent's own next scheduled check-in asks "what should I be
running" and picks it up. The web UI's "pending" vs. "applied" badge
exists specifically to make that asynchrony visible rather than
implying something more immediate.

## Wire shape: `AgentControl.CheckIn`

New proto (`proto/sentry/agent/v1/agent_control.proto`), a second gRPC
service on the exact same mTLS channel/listener `LogIngest.PushBatch`
already uses — not a second protocol or connection the agent has to
maintain. Called on the agent's own heartbeat ticker (see
`agent/sentry-agent/src/main.rs`'s `heartbeat_ticker` arm),
independently of whether the heartbeat log record itself is enabled —
CheckIn keeps running even with `heartbeat.enabled = false`, since
that's an agent's only path to ever receive a remote override that
re-enables it.

- **Request**: host/service identity, a `ReportedConfig` snapshot of
  what the agent is actually running (version, source kind/detail,
  batch settings, heartbeat settings), and the version of the last
  override this agent successfully applied (empty if never).
- **Response**: `has_override` plus, when true, a `DesiredOverride` —
  every field optional (unset = "no change to this field, keep local
  config"), each independently overridable.

## What's remotely editable, and what deliberately isn't

Editable: `batch.max_size`, `batch.flush_interval_ms`,
`heartbeat.enabled`, `heartbeat.interval`, and — only when the agent's
local source is journald — the unit filter.

**Never editable, permanently**: TLS material and the ingest endpoint.
`ReportedConfig` doesn't even carry these fields, and
`DesiredOverride` has no fields for them at all — this isn't a
validation rule that could be relaxed later, it's a shape decision.
Two reasons, both serious enough that this needed to be a design
boundary rather than a judgment call per edit:

1. **A bad edit could permanently strand an agent.** Point an agent's
   `ingest.endpoint` at an address that doesn't exist, or corrupt its
   TLS config, and it can never call `CheckIn` again to receive a
   correction — the one channel capable of fixing the mistake would be
   exactly what broke. Every other editable field is safe by
   construction: even a bad heartbeat interval or an over-aggressive
   batch size degrades the agent's behavior without ever cutting off
   its ability to receive the next correction.
2. **A compromised web session/API credential must not be able to
   redirect where an agent's logs go.** If `ingest.endpoint` were
   editable, an attacker with write access to this feature could point
   agents at an address they control and exfiltrate log data. Keeping
   connection details local-file-only means this feature's blast
   radius is "an agent's operational tuning gets messed with," never
   "an agent's data goes somewhere else."

`validateOverride` (`api/agents/handler.go`) additionally floors
`batch_max_size >= 1`, `batch_flush_interval_ms >= 100`, and
`heartbeat_interval_ms >= 5000` — the same kind of real, found-by-
building-it floor as alerting's `eval_interval_seconds >= 30`, here to
stop a fat-fingered edit from telling an agent to flush constantly or
heartbeat constantly.

## Merge semantics: an override is a live layer, not a rewrite

The agent's local `agent.toml` is never rewritten. A remote override
lives only in the running process's memory
(`agent/sentry-agent/src/main.rs`'s `apply_override`) and is re-applied
fresh on every check-in that returns one — a restarted agent boots from
`agent.toml` alone and re-syncs whatever override is still set on its
next successful check-in. This was a deliberate simplicity choice over
persisting the override to disk: it avoids needing filesystem write
access on every managed host (not guaranteed, e.g. read-only base
images) and avoids a whole "reconcile a locally-cached override against
a freshly-fetched one at startup" state machine. The cost is that an
agent's effective config isn't fully recoverable from `agent.toml`
alone while an override is active — acceptable, since the web UI's
`GET /agents/{host}` is the source of truth for "what is this agent
actually running" regardless.

Applying an override that changes `batch_max_size`/
`batch_flush_interval_ms` rebuilds the `Batcher`/flush ticker outright.
Whatever was buffered under the old settings is flushed first
(`Batcher::flush_all`, new) rather than dropped — building this exposed
a real, independent, pre-existing bug: agent shutdown was calling
`poll_timeout()`, which only drains when `flush_interval` has already
elapsed, meaning records buffered more recently than that were silently
lost on every graceful shutdown that happened to land between flushes.
Fixed alongside this feature (`flush_all()` now used at both shutdown
and hot-reload) since it's the exact same correctness property in both
places.

Changing the journald unit filter is the one override that can't just
swap a struct field — there's no way to change what
`source::journald::run` is tailing without restarting that task. Applying
it aborts the current source task and respawns a fresh one with the new
filter, swapping the channel `main.rs` reads from. Source *kind*
(journald vs. file vs. eventlog vs. etw) is never remotely switchable —
only narrowing/widening the filter within whatever source the host is
already configured for.

## Data model

`metadata/migrations/0037_create_agents.sql`: one `agents` table, one
row per `(tenant_id, host)`, in the same `sentry_metadata` Postgres
dashboards/alert_rules already live in — not a new database, matching
this project's established "shared schema, different services own
different tables" shape. `tenant_id` defaults to `'default'` for
single-tenant deployments with no `TenantResolver` configured on
ingest, same as every other tenant-scoped table since Phase 3.

Two services write to it, cleanly split by concern:

- **`ingest/internal/agentregistry`** (new) upserts on every `CheckIn`:
  `last_seen_at`, the `reported_*` columns, and
  `applied_override_version` (echoed from the agent). Reads back
  whatever `desired_override`/`desired_override_version` is currently
  stored to answer the RPC. Gated on `AGENT_REGISTRY_POSTGRES_ADDR`
  being set — nil/off by default, same "off unless configured" shape as
  `TenantResolver`; a deployment that hasn't opted in still accepts
  `CheckIn` calls (agents never see an error), it just doesn't record
  anything or ever return an override.
- **`api/agents`** (new) is the web-facing read/write side: `GET
  /agents`, `GET /agents/{host}`, `PUT /agents/{host}/config` (replaces
  the whole stored override — the web UI's edit form always reads the
  current override first and submits the complete merged set, same
  "PUT replaces the resource" convention every other edit form in this
  codebase already uses), `DELETE /agents/{host}/config` (clears it,
  reverting the agent to its local `agent.toml`).

`ConfigOverride`'s JSON shape is duplicated three times — Go structs in
`ingest/internal/agentregistry` and `api/agents`, a proto message for
the wire — deliberately, matching this codebase's established
convention for shapes shared across module boundaries (see
`grpcserver.TenantIDHeaderKey`, `enterprise/internal/apiconfig.AIConfig`)
rather than coupling independently deployable services' builds
together. Keep the three in sync by hand.

## RBAC

Viewing inventory is `RoleViewer` (same bar as viewing a dashboard);
editing an agent's remote config is `RoleEditor` — treated as an
operational-tuning action matching alert rules/notification targets,
not an admin-only capability like user/role management. Issuing a
lifecycle command is `RoleAdmin` — stricter, matching the RBAC matrix's
treatment of similarly consequential actions (e.g. deleting a
notification target) rather than day-to-day tuning.

## Lifecycle commands (punch-list item 1: restart)

A one-shot action, not a persistent desired state like `DesiredOverride`
-- `AgentCommand` (proto enum, `agent_control.proto`) is delivered
**at-most-once**: `ingest/internal/agentregistry.Registry.CheckIn`
clears `agents.pending_command` in the same transaction that hands it
to the agent, before the agent has any chance to confirm it executed
the command. This is a deliberate, disclosed asymmetry from
`DesiredOverride`'s "keep re-offering until the agent's
`applied_override_version` matches" semantics: a restarting agent's
process is gone before it could ever send that confirmation, so waiting
for one isn't possible. A command lost to a network blip between the
response and the agent acting on it is simply lost -- re-issuing (`PUT
/agents/{host}/command` again) is the operator's recourse, the same as
it would be for a `systemctl restart` that silently failed to reach its
target.

Only `restart` exists today. `stop` and `uninstall` remain deliberately
deferred -- both need real OS service-manager integration (systemd's
`Restart=`/`RestartPreventExitStatus=` semantics vs. Windows SCM
recovery options differ enough per platform that hand-waving them would
be dishonest), which `restart` doesn't: the agent does a graceful
shutdown (flushing whatever's buffered via `Batcher::flush_all`,
aborting the source task) and then a clean `std::process::exit(0)`,
relying entirely on whatever restart policy the host's service manager
already has configured -- the same contract any well-behaved service
already expects. `api/agents.Store.IssueCommand`/the `agents` table's
`pending_command` column are written generically enough that adding
`stop`/`uninstall` later is a real per-platform agent-side
implementation, not a data-model change.

Logged into the same append-only `audit_log` table as everything else
privileged in this codebase (`event_type = 'agent_command'`,
`metadata/migrations/0039`), via `enterprise/internal/audit.
AgentCommandLogger` (`api/agents.CommandLogger`, nil-by-default in core
same as every other optional audit hook) -- fail-open, same posture as
Phase 7's AI-interaction logging: a write failure is logged server-side
and never turns a legitimate restart into a 500.

**A real bug was found and fixed while verifying this live.** The
first version of `agentregistry.Registry.CheckIn` tried to read-and-
clear `pending_command` atomically using a single `INSERT ... ON
CONFLICT` statement with a sibling read-only CTE referenced only from
`RETURNING`, on the assumption that Postgres evaluates every part of a
`WITH` query against the same pre-statement snapshot. That assumption
is wrong specifically for `FOR UPDATE`: it always locks (and therefore
reads) the *latest* row version to do its job, including a version
written earlier in the very same statement -- confirmed empirically
against a live Postgres (the CTE's `FOR UPDATE` was reading its own
sibling `UPDATE`'s just-cleared `NULL`, so `pending_command` always
came back empty even when a command was genuinely pending, and the
agent never received it). Live verification caught this immediately: a
restart command showed `pending_command: "restart"` in the API
response, but the agent never logged receiving it and never exited.
Fixed by splitting into two real, ordered statements inside one
explicit transaction (`SELECT ... FOR UPDATE` strictly happens-before
the clearing `UPDATE`) -- unambiguous, no snapshot-timing subtlety to
get wrong.

## Verified live

A real agent binary, its heartbeat/CheckIn cadence pointed at a live
`ingest` with `AGENT_REGISTRY_POSTGRES_ADDR` configured, confirming: the
agent appears in `GET /agents` after its first check-in; an edit made
via `PUT /agents/{host}/config` shows `pending: true` immediately and
`pending: false` after the agent's next check-in; the edited setting
(heartbeat interval) visibly takes effect in the agent's own behavior,
confirmed by the change in cadence of new heartbeat rows landing in
ClickHouse; a journald-unit override triggers a real source-task
restart, reflected in the next reported `source_detail`; and (after the
bug above was fixed) a restart command issued via `PUT /agents/{host}/
command` is picked up on the agent's very next check-in, logged
(`"received remote restart command, shutting down gracefully"`), and
the process exits cleanly -- `pending_command` confirmed cleared and
`ps` confirming the process gone.

## CLI surface (punch-list item 3)

`sentryctl agents` (`cli/cmd/sentryctl/cmd_agents.go`), same list/get
shape as `dashboards`/`alerts`, plus a `config` sub-subcommand
(mirroring `dashboards permissions`) since an override has its own
get/set/clear lifecycle distinct from the agent resource itself:

```
sentryctl agents list|get <host>
sentryctl agents config get <host>|clear <host>
sentryctl agents config set <host> [--batch-max-size N] [--batch-flush-interval-ms N]
                            [--heartbeat-enabled true|false] [--heartbeat-interval-ms N]
                            [--journald-unit UNIT]
sentryctl agents restart <host> [--yes]
```

`config set` is the one command with real logic beyond a thin HTTP
wrapper: since `PUT /agents/{host}/config` replaces the whole stored
override (not a per-field patch), the command fetches the agent's
current effective config first and merges only the flags actually
passed on top of it -- whatever's already overridden stays overridden,
whatever isn't falls back to the agent's reported value -- exactly
mirroring `web/src/routes/agents/[host]/+page.svelte`'s edit form logic
in Go instead of Svelte. `restart` requires explicit confirmation
(interactive y/N, or `--yes` for scripted use) and refuses outright on
a non-interactive stdin without `--yes`, the same posture
`cmd_query.go`'s `--nl`/`--execute` already established for anything
that can actually change what's running.

**Verified live**, including the merge logic specifically (the part
most likely to have a real bug): `config set --heartbeat-interval-ms
20000` on an agent with no existing override correctly carried forward
its reported `batch_max_size`/`batch_flush_interval_ms`/
`heartbeat_enabled`; a second `config set --batch-max-size 750` call
correctly carried forward the *first* call's `heartbeat_interval_ms:
20000` override rather than resetting it to the reported `5000` --
confirming the read-current-then-merge step actually reads the
override, not just the reported baseline. `config clear` and `restart
--yes` both round-tripped against the live stack; the restart was
picked up by the real agent process on its next check-in, logged, and
the process exited cleanly, closing the loop from a single CLI command
all the way to real process behavior.

## Punch list: complete

All three items from `/docs/agent-heartbeat-monitoring.md`'s original
punch list are done: lifecycle commands (restart), fleet-wide alerting
(via the existing raw-SQL escape hatch, no engine changes), and this
CLI surface. Real, disclosed remaining gaps, not oversights: `stop`/
`uninstall` lifecycle commands (need genuine per-platform OS service-
manager integration), true per-host multi-row alerting from one rule
(needs the alerting engine's own per-group state tracking, a Phase 3
non-goal for the whole engine, not agent-specific), and a scripted
rule-per-host generator (a real alternative to the fleet-wide aggregate
check, not built since it's tooling that could live under this same CLI
surface if ever wanted).
