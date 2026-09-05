# Phase 8 processing design: rules, where they run, and how they arrive

> **Status:** Design, drafted 2026-09-05, **not implemented** — nothing
> in Phase 8 is built. Four decisions are settled and dated: the rule
> shape, total evaluation with apply-then-verify, `regex-lite` on the
> agent, and shipping without a canary. Two remain genuinely open — rule
> placement, and what `aggregate_count` emits. The specification itself
> is the conformance corpus in [`/processing`](../processing/README.md);
> this document is a summary of it and loses any argument between them.
> If implementation shows this is wrong somewhere, fix this doc in the
> same change rather than letting them drift.

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

**Decided 2026-09-05: apply-then-verify ships in v1.**

It was a defensible cut while a canary might have backstopped a bad
rollout. With no canary (Decision 4), it is the only thing that recovers
a host without an operator noticing, and "total evaluation should make
it unreachable" is what every crash-loop was before it happened.

### What persists, exactly

A single small file next to the agent's config, holding two fields:

```
override_version   the version stamp the agent is currently attempting
state              "trying" | "good" | "quarantined"
```

The rule set itself is **never** written. That is what keeps the
crash-loop-not-strand property intact: an agent that loses this file, or
never had it, still boots clean and re-syncs.

The sequence on receiving a rule set with a new version:

1. Write `{version, "trying"}` and fsync **before** applying anything.
2. Apply the rules.
3. On the next successful check-in, rewrite as `{version, "good"}`.

On boot, the agent reads the file:

- absent, or `good` — normal start, apply whatever the next check-in
  returns.
- `trying` — the previous process died while carrying that version.
  Rewrite as `{version, "quarantined"}`, start with **no rules**, and
  report the quarantined version on the next check-in so it is visible
  rather than merely survived.
- `quarantined` — keep refusing that exact version. Any *different*
  version clears the quarantine and is tried normally, because the
  operator pushing a new rule set is the correction.

### Failure modes, decided rather than discovered

**The agent cannot write the file** (read-only image, full disk). It
logs once, runs with total evaluation alone, and applies rules normally.
Degrading to "no rules" would punish every read-only deployment for a
failure that has not happened; degrading to "apply anyway" is what the
mechanism already does minus the recovery. The backstop is best-effort
by construction, and the doc should not pretend otherwise.

**The agent dies for an unrelated reason** while carrying a good rule
set — OOM from something else, a host reboot, a `kill -9`. It
quarantines a blameless rule set. This is a false positive by design:
the alternative is distinguishing "died because of the rules" from "died
while the rules happened to be loaded", which the agent cannot do
honestly. One unnecessary quarantine, visible on the Agents page and
cleared by re-pushing, is a much better error than one missed real one.

**A rule set is fatal only on some hosts** — a pattern that behaves
badly against data only one host sees. Each agent quarantines
independently, which is the correct behaviour and also the closest thing
to a canary this design has: the first host to hit it quarantines and
reports while the others carry on.

### The conformance corpus cannot test this

Worth stating so nobody tries. The corpus is records in, records out —
it pins rule *semantics*. Apply-then-verify is agent lifecycle
behaviour: process death, file state across restarts, and what gets
reported on the next check-in. None of that is expressible as an input
record and an expected output record.

It needs its own tests, on the agent side, driving a real process
through crash and restart — closer in shape to how
`agent-management-design.md`'s restart command was verified live than to
anything in `/processing`.

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

**Decided 2026-09-05: `regex-lite` on the agent, full `regex` at
ingest, conformance corpus restricted to the syntax both accept.**

The agent had no regex dependency at all, and the full `regex` crate is
a megabyte-plus against an agent whose pitch is a small static musl
binary. The rejected alternative was regex ingest-side only, which would
have sacrificed the "redact PII before it leaves the host" claim in
`positioning.md` — the one capability that most needs to be on the
agent, and the reason on-host processing exists.

Checked before committing to it rather than assumed: every pattern the
corpus uses compiles and behaves under `regex-lite` — `{n}` quantifiers,
alternation, named captures (`(?P<name>…)` yields the expected capture
names), and `replace_all` replacing *every* occurrence, which is what
`mask` requires and what a redaction that stopped at the first hit would
get wrong.

Both engines resolve alternation leftmost-first, so the two
implementations agree on which branch wins. The corpus pins this rather
than trusting it to stay true.

The constraint this creates: **the corpus may only use syntax
`regex-lite` supports.** Unicode-aware character classes and the richer
Perl classes are out, in both implementations, because a case the agent
cannot run is not a conformance case.

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

**Decided 2026-09-05: no canary gate. Ship without staged rollout.**

An edit reaches every matching agent on its next check-in. For
executable rules that is the difference between breaking one host and
breaking all of them, and this design recommended a canary might have to
gate the feature. It does not.

The risk is accepted rather than dismissed, and it is worth being exact
about what carries it:

- **Total evaluation** is meant to make a fatal rule set impossible to
  express in the first place. That is the actual mitigation; everything
  below is what happens when it fails.
- **A fatal rule set crash-loops rather than strands**, because
  overrides are never persisted. Every agent it reached keeps checking
  in and can be corrected in one edit.
- **Apply-then-verify** makes that self-healing rather than
  operator-driven: an agent that crash-loops falls back to running
  without rules on its own.
- **`applied_override_version` already shows the blast radius.** An
  operator can see how many hosts have taken a rule set, which is a
  canary's observability without a canary's machinery.

What is genuinely given up is the chance to *stop* a bad rollout partway
through. Everything above shortens the outage; none of it prevents the
rule reaching every host first. A canary remains the right thing to
build later, and is now a candidate for a follow-up rather than a
blocker.

This also raises the stakes on the first item. Without a canary, total
evaluation is not a nice property of a well-designed DSL — it is the
only thing standing between a bad rule and every host at once. That
makes open question 1 below considerably less optional than it looked
when it was written.

## Decision 5: what `aggregate_count` emits

Deferred twice because it is the only action whose output is not simply
the input with edits. It emits a record that never existed.

**Shape: the window's first record, tagged.** The codebase already has a
convention for synthetic records and this follows it rather than
inventing a second one — heartbeat and host-metrics records are ordinary
`LogRecord`s distinguished by a `cairnobs.heartbeat` /
`cairnobs.metrics` attribute, with `service` left as the agent's real
service. So an aggregate is the first record of its window, unchanged,
plus:

| attribute | value |
|---|---|
| `cairnobs.aggregated` | `"true"` |
| `cairnobs.count` | number of records collapsed, as a string |
| `cairnobs.window_start_unix_nano` | timestamp of the first record |
| `cairnobs.window_last_unix_nano` | timestamp of the last contributing record |

Keeping the first record intact means a human reading the line sees a
real example of what was collapsed, not a summary someone invented.

**It tags even when the count is one.** Emitting a bare record for a
window that happened to see one event would be tidier and is wrong: it
makes `cairnobs.count` present only sometimes, so the correct way to
count aggregated data silently breaks on quiet windows. Uniformity beats
tidiness here.

**Window bounds are observed, not nominal.** `window_last` is the last
record that actually contributed, never `window_start + window_ms`. A
window flushed early must not claim an end that never happened.

### When it emits, and the problem that hides here

Windows are measured on record time (see `/processing/README.md`), which
means a window can only be *closed* by a later record arriving. That is
fine for `suppress_duplicates`, which emits the first record immediately
and drops the rest — nothing is ever pending.

`aggregate_count` holds state. If the matching stream goes quiet, the
pending aggregate has nothing to close it and sits unemitted, possibly
for hours. That is data loss dressed as latency, and it is the real
reason this action was harder to specify than the other nine.

So emission has two triggers:

1. **A later matching record** with a timestamp at or past
   `window_start + window_ms`. The pending aggregate is emitted first,
   then that record opens the next window.
2. **End of stream** — agent shutdown, and in production the batch flush
   interval. Anything pending is emitted.

**The cost, stated plainly:** trigger 2 is wall-clock in production,
which means `window_ms` is a *maximum*, not a guarantee, and one logical
burst can produce more than one aggregate record if a flush lands in the
middle. Consumers must treat aggregates as additive — which they already
must, since a burst can span windows anyway.

The conformance corpus defines trigger 2 as an implicit flush after the
last input, which keeps the cases deterministic while describing real
behaviour.

### The consequence nobody should discover in production

**`stats count` undercounts aggregated data**, silently. A hundred
events become one record, so every existing dashboard panel and alert
rule that counts rows changes meaning the moment a rule starts
aggregating the data behind it.

Nothing in this design fixes that, and pretending otherwise would be
worse than saying it. Three things follow:

- The correct idiom over aggregated data is summing `cairnobs.count`,
  not counting rows. That belongs in the query language reference before
  this action ships.
- Teaching the query layer to do it automatically — making `count`
  mean "sum `cairnobs.count` where present" — is a Phase 2 change to the
  IR and executor, not a Phase 8 change, and it is the right long-term
  answer. **Open**, and it should be decided before aggregation is
  recommended for any data an alert already watches.
- It is another argument for aggregation staying opt-in per rule, which
  it is.

**Choosing between the two dedup actions:** `suppress_duplicates` is
cheaper and loses the count; `aggregate_count` preserves it and creates
a synthetic record with all of the above attached. Use suppress when the
repetition is noise, aggregate when the rate is the signal.

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

1. ~~Apply-then-verify in v1?~~ **Decided 2026-09-05:** yes. With no
   canary, it is the only thing that recovers a host unattended. The
   persisted state, its failure modes, and why the conformance corpus
   cannot cover it are specified in Decision 1.
2. ~~Regex on the agent?~~ **Decided 2026-09-05:** `regex-lite` on the
   agent, full `regex` at ingest, corpus limited to their common syntax.
3. ~~Canary rollout?~~ **Decided 2026-09-05:** no gate, ship without it;
   a canary is follow-up work.
4. Explicit per-rule placement, or platform-decided?
5. ~~What does `aggregate_count` emit?~~ **Decided 2026-09-05:** the
   window's first record tagged with `cairnobs.aggregated`,
   `cairnobs.count` and observed window bounds — see Decision 5. It
   raises one new question in its place: whether `stats count` should
   learn to sum `cairnobs.count` automatically, which is a Phase 2
   change to the IR rather than a Phase 8 one, and should be settled
   before aggregation is pointed at data an alert already watches.
