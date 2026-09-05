# Phase 8 processing design: rules, where they run, and how they arrive

> **Status:** Design, drafted 2026-09-05. **Not approved and not
> implemented.** Nothing in Phase 8 is started. This is a proposal to
> argue with — the sections marked **Open** are genuine decisions, not
> rhetorical ones. If implementation shows this is wrong somewhere, fix
> this doc in the same change rather than letting them drift.

## Why this design, in one paragraph

Phase 8 puts rule-based work on records in flight: drop, mask, rename,
derive, parse, sample, suppress duplicates, aggregate. The hard part is
not the transformations — it is that the same rule has to mean the same
thing in a Rust agent and a Go ingest tier, and that rules are pushed to
hosts over a channel deliberately built to be incapable of carrying
anything dangerous. So this design starts from the distribution channel
and the safety invariant it protects, and derives the language from
them, rather than designing a language and asking later how to ship it.

## The constraint everything else follows from

[`agent-management-design.md`](agent-management-design.md) states an
invariant plainly: every remotely editable field "degrades the agent's
behavior without ever cutting off its ability to receive the next
correction." That is why `ingest.endpoint` and TLS material are
permanently non-editable — a bad value there kills the only channel that
could fix it.

Processing rules are the first remotely editable thing that can execute.
A rule that panics, loops forever, or allocates without bound is not a
degraded setting; it is a broken agent.

### A correction to what the roadmap says

The roadmap change in #21 stated that a bad rule "strands the agent
exactly the way a corrupted `ingest.endpoint` would." Reading
`apply_override`'s actual semantics, that is too strong, and the
difference matters enough to write down rather than quietly soften:

**An override lives only in the running process's memory.** It is never
written to `agent.toml`. A restarted agent boots from its local config
alone and re-syncs on its next successful check-in.

So a rule set that crashes the agent produces a **crash-loop**, not a
strand:

```
boot (clean, no rules) → CheckIn → receive rules → apply → crash → boot …
```

The agent checks in on every iteration of that loop. The platform can
always push a corrected or cleared override, and the agent will take it.
That is a materially better failure mode than being stranded, and it
exists by accident — the "don't persist overrides" choice was made for
simplicity (no filesystem writes on read-only base images, no
reconcile-at-startup state machine), not for safety.

**This design promotes that accident to a constraint.** Persisting
overrides to disk would convert every crash-loop into a strand, because
the agent would apply the fatal rules before its first check-in and never
reach one. Anyone proposing offline-boot override persistence later must
solve this first. It is now load-bearing.

The residual harm is still real and still worth engineering away: a
crash-looping host ships almost nothing, and the loop runs at whatever
the check-in cadence is until a human notices.

## Decision 1: total evaluation, with apply-then-verify as a backstop

Two candidate guarantees were named in #21. This design takes **both**,
in priority order, because they solve different halves.

**Total evaluation** — the rule language is constructed so a rule set
*cannot* panic, loop unboundedly, or allocate without limit. This is the
primary guarantee, and it is an absence of the failure rather than a
recovery from it. It is purchasable only by keeping the language
declarative and typed, which the next section does.

**Apply-then-verify** — the agent treats a newly received rule set as
provisional: it records the version it is about to apply, applies it, and
marks it good once it has survived one full check-in interval. If it
boots and finds a provisional version recorded that never went good, it
reports the failure and runs *without* rules rather than reapplying them.

Note the tension with the constraint above: apply-then-verify needs a
small amount of state to survive a restart, which is exactly the
persistence the previous section forbids. The resolution is that what
persists is **a version stamp and a failure flag, never the rule set
itself** — a few bytes, and a host that cannot write even that simply
loses the backstop and keeps total evaluation. The agent must degrade to
"no rules" on a write failure, never to "apply anyway".

**Open:** whether the backstop is worth its complexity in the first
release, given total evaluation should make it unreachable. My view is
yes — "should be unreachable" is what every crash-loop was before it
happened — but it is a defensible cut for a v1.

## Decision 2: the rule shape

A rule is a **matcher** and an ordered list of **typed actions**. No
expressions, no arbitrary code, no user-supplied control flow.

```
rule
  match:   field, operator, value        (all must hold)
  actions: [ action, action, … ]         (applied in order)
```

Actions, and whether each is trivially total:

| Action | Effect | Total? |
|---|---|---|
| `drop` | discard the record | yes |
| `drop_fields` / `keep_fields` | remove or whitelist attributes | yes |
| `mask` | replace matched substring with a fixed token | yes, with a linear-time engine |
| `rename` | move a field | yes |
| `derive` | set a field from a literal or another field | yes |
| `parse_json` | parse `message` into fields | yes, with a depth and size cap |
| `parse_regex` | named captures into fields | yes, with a linear-time engine |
| `sample` | keep 1 in N | yes |
| `suppress_duplicates` | collapse identical records within a window | yes, with a bounded cache |
| `aggregate_count` | replace repeats with a count record | yes, with a bounded cache |

Deliberately absent: arbitrary expressions, loops, user-defined
functions, and anything resembling `eval`. Cribl's rule language is
JavaScript; this is less expressive on purpose. It is also the only
shape that can be pushed to ten thousand hosts and audited by reading it.

### Why regex does not break totality

Both implementation languages ship linear-time, non-backtracking regex
engines — Rust's `regex` crate and Go's `regexp` are both
finite-automata based, with no catastrophic backtracking to guard
against. This is a real piece of luck: the usual reason regex is unsafe
in a pushed rule set does not apply here, in either language,
without doing anything clever.

**Open, and it has a cost:** the agent currently has **no regex
dependency at all**. Adding the full `regex` crate is on the order of a
megabyte-plus of binary, against a project whose agent pitch is a small
static musl binary. `regex-lite` is far smaller and still linear-time,
at the cost of some syntax and speed. Alternatively `parse_regex` and
`mask` could be ingest-side only in v1, which sacrifices the "redact PII
before it leaves the host" claim in `positioning.md` — the one thing
that most needs to run on the agent. My recommendation is `regex-lite`
on the agent and full `regex` at ingest, with the conformance suite
(below) restricted to the syntax both accept.

## Decision 3: one spec, two implementations, one conformance suite

Rules run in Rust on the agent and in Go at ingest. "The same rule does
the same thing in both places" is the whole promise, and two
hand-written implementations will diverge — not maybe, eventually.

The deliverable that prevents it is a **language-neutral conformance
suite**: a directory of cases, each a rule set, a sequence of input
records, and the records expected out, in JSON. Both implementations run
it in their own CI. A case is added for every bug found in either.

This is the same discipline `/hack`'s fixtures already apply to ingest
shapes, applied to semantics instead. It should be built **first**, not
last — the suite is the specification, and the prose above is a summary
of it.

**Built:** [`/processing`](../processing/README.md), 38 cases. Nothing
executes them yet, since neither implementation exists; a structural
validator runs in CI so the corpus cannot rot in the meantime. Writing
the cases first has already paid for itself — it forced two determinism
decisions that prose had left vague (see that README's "Two determinism
decisions the suite forces"), and it made the absence of an
`aggregate_count` answer concrete enough that the validator rejects any
case using it.

## Decision 4: distribution reuses the channel that exists

Fleet management already delivers desired state
([`agent-management-design.md`](agent-management-design.md)). Rules
become one more field on `DesiredOverride`:

```protobuf
repeated ProcessingRule rules = 8;
```

`extra_file_paths = 7` is the precedent to follow exactly: a repeated
field with no meaningful "unset", where the platform always submits the
complete desired list and an empty list unambiguously means "no rules
right now". The existing `version` stamp and `applied_override_version`
echo give rollout observability for free — you can already see which
hosts have taken a rule set and which have not.

**Open:** there is no staged rollout today. An edit goes to every agent
matching it on their next check-in. For batch sizes that is fine; for
executable rules it is the difference between breaking one host and
breaking all of them. A canary mechanism — apply to N hosts, require
them to report good, then widen — is not in the fleet design and would
be new work. My view is that this is the single most important thing to
add alongside rules, and it may deserve to gate the feature.

## Where each rule runs

Agent-side is the default and the cheaper place: data reduced before the
wire costs nothing to transport, store or index, and it is the only place
PII can be removed before it crosses the network.

Ingest-side exists for rules needing context the agent lacks, and for
changing behaviour without waiting for a fleet rollout.

**Open:** whether a rule declares where it runs, or whether the platform
decides. Explicit placement is simpler to reason about and to debug;
automatic placement is friendlier and much easier to get subtly wrong. I
lean explicit, with a validation error when a rule asks for something its
location cannot do.

## The motivating workload is real

The maintainer's own workstation, measured 2026-09-05 while the dev agent
ran against a live stack:

| rate | source | message |
|---|---|---|
| ~39/min | Discord | `Discord 1.0.155`, byte-identical every time |
| ~22/min | foreground_boost | `Checking active tasks` |

Those two are **308 of 325 journal entries in five minutes** — roughly
60% of one host's volume, from two processes saying nothing. Everything
else on that host, kernel firewall drops included, is single digits.

Today there is no way to do anything about it: the journald source's only
filter is a single-unit allowlist, so the choice is the whole journal or
one unit, with nothing in between.

**This is the v1 acceptance test.** A `suppress_duplicates` rule and a
`drop` rule should remove ~60% of that host's volume, measured
before-and-after against real data rather than a fixture. If the first
release cannot do that, it is not finished.

## Open questions, collected

1. Is apply-then-verify in v1, or is total evaluation alone enough?
2. `regex-lite` on the agent, full `regex` at ingest, and a conformance
   suite limited to their common syntax — or regex ingest-side only,
   giving up on-host redaction for v1?
3. Does a canary rollout gate the feature, or ship after it?
4. Explicit per-rule placement, or platform-decided?
5. Does `aggregate_count` emit a synthetic record, and if so what does it
   look like to a query that is not expecting one? This design does not
   answer that and should before anyone builds it.
