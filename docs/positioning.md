# Positioning: Splunk and Cribl

Cairn OBS has always been positioned against Splunk. It is now also
positioned against Cribl. Those are not the same claim, and holding both
honestly changes what this project has to build.

This document reconciles them, and derives the feature and roadmap
consequences. It is the argument; `/docs/status.md` is the record of what
is actually built.

## They are not the same competitor

**Splunk is a destination.** Data lands in it, is indexed, searched,
dashboarded and alerted on. Cairn OBS replaces it: same job, different
storage economics. Every phase through 7 was built for that fight, and
that positioning is unchanged.

**Cribl is the road to the destination.** Cribl Stream sits between the
sources and wherever the data is going, and routes, reduces, enriches,
redacts, transforms and replays it on the way. Cribl Edge manages the
agent fleet that feeds it. Neither is a place data lives — they are
control over data in motion.

So "we compete with Splunk and Cribl" is not one claim made twice. It is
a claim about the destination and a claim about the road.

## The awkward part, stated plainly

**Most people buy Cribl because Splunk is expensive per gigabyte.** The
pipeline pays for itself by dropping, sampling and trimming data before
it reaches a licence priced by volume.

That creates a tension a cost-led project has to face rather than paper
over:

- If Cairn OBS is genuinely cheap per GB, the main reason to buy Cribl
  *for Cairn OBS* is gone. Replacing Splunk with something cheap removes
  the need for the tool that exists to make Splunk affordable.
- Which means the strongest combined pitch is **one system where there
  were two** — not "we are also a pipeline vendor".
- But that pitch only survives contact with a buyer if Cairn OBS also
  does the things people buy Cribl for that are *not* about cost.

Those things are real, and they do not go away when storage gets cheap:

| Reason to run a pipeline | Cheaper storage makes it… |
|---|---|
| Cut volume to fit a licence | mostly moot |
| Route one stream to several destinations | unchanged |
| Redact PII/PCI *before* data leaves the network | unchanged |
| Keep an auditable archive and replay from it | unchanged |
| Avoid lock-in to any one analytics vendor | unchanged |
| Manage agent config across a fleet | unchanged |

Four of those six are about **control**, not spend. That is the ground
Cairn OBS has to compete on, and it is ground worth taking: control is a
better story than cost anyway, because cost advantages get matched and
control advantages are architectural.

## The uncomfortable consequence

To compete with Cribl at all, Cairn OBS has to be able to **send data to
other vendors' systems** — S3, Splunk HEC, Elastic, OTLP, Kafka, another
SIEM. That means building features whose explicit purpose is to help data
leave this platform.

Most vendors will not do that, which is exactly why it is worth doing. It
is also consistent with what this project already is: AGPLv3 throughout,
no commercial-license wall, no proprietary storage format. A project that
refuses lock-in in its licence and then builds it into its egress would
be lying about itself.

It should be stated as a deliberate decision rather than discovered later
as a surprise: **Cairn OBS will make it easy to send your data somewhere
else, including to a competitor.**

## What exists today

The data path is already a pipeline in shape. It exposes none of the
controls of one.

```
agent (Rust)                     ingest (Go)
sources ─► parse ─► batch ─► mTLS gRPC ─► Redpanda ─► normalize ─► ClickHouse
journald                                                        └► Tantivy
file tail
Event Log / ETW
```

- **The agent** reads, parses RFC 5424 where it applies, batches and
  ships. It cannot filter, drop, sample, mask, enrich or re-route
  anything.
- **Ingest** normalises the wire record into the ClickHouse row shape and
  writes it. `internal/normalize` is the only per-record processing that
  exists, and it is a schema mapping, not a rule engine.
- **There is exactly one destination**, and it is us.

Redpanda sits in the middle of that path already, which is the natural
seam for stream processing. Nothing uses it that way yet.

## What this adds to the feature set

Grouped by how much is genuinely new versus how much is exposing what the
architecture already has.

### 1. A processing pipeline — the substantial one

Rule-based work on records in flight: drop and keep fields, mask and
redact, rename, derive, parse (regex/grok/JSON into fields), sample,
suppress duplicates, and aggregate repetitive events into counts.

**The design decision that has to be made first: where it runs, and in
what language.**

Running it *on the agent* is the cheapest possible place — data reduced
before the wire costs nothing to transport, store or index, and it is the
only place PII can be removed before it crosses the network. It is also
where this project has a structural advantage: the agent is a
statically-linked musl Rust binary, where Cribl Edge is considerably
heavier.

But it collides with a non-negotiable constraint. Cribl's rule language
is JavaScript; embedding a JS engine in the agent would end "no glibc
runtime deps, one static binary" as a claim. **The recommendation is a
declarative rule DSL** — matchers and typed actions, no arbitrary code —
serialised into the agent config. Less expressive than Cribl on purpose:
smaller, auditable, safe to push to ten thousand hosts, and impossible to
turn into a remote-code-execution surface.

Central processing at the ingest tier is the complement: rules that need
context the agent lacks, and a place to change behaviour without a fleet
rollout.

### 2. Routing and multiple destinations

Conditional routing — this source, matching this rule, to these
destinations. Needs per-destination retry, backpressure and delivery
accounting, which is a materially harder problem than one destination
that is always us. Sinks worth having: object storage, Splunk HEC,
Elastic bulk, OTLP, Kafka, plain HTTP.

### 3. Archive and replay

An archive format on object storage, and the ability to read it back into
the pipeline or into a destination later. This is the feature that makes
aggressive reduction safe: you can drop something from the hot path
precisely because you can get it back. It also folds in the retention/TTL
question `/docs/architecture.md` currently lists as unresolved and
deferred — that question stops being deferrable here.

### 4. Fleet management

Central agent configuration: author, version, roll out, and observe. Much
of the substrate exists — agents check in, report their own version and
source config, and there is an Agents page that already knows when one
goes stale. What is missing is the direction of travel: config currently
flows *to* the agent from the host, not from the platform.

### 5. Schema normalisation as a feature, not a detail

OTel semantic conventions are already the stated default schema. Mapping
between OTel, ECS and Splunk CIM is what makes a router useful rather
than merely functional — it is the difference between forwarding bytes
and delivering something the destination understands.

### 6. Search in place — noted and not proposed

Cribl Search queries object storage without ingesting first. It is a
genuinely different execution model to the one in
`/docs/architecture.md`, and adopting it would be a second storage engine
rather than a feature. Recorded here so the omission is visible, not
because it is next.

## Roadmap consequence

Phases 0–7 built the destination. This is a second axis, not a
continuation of the first, and it is worth numbering separately rather
than appending forever to a list that was about analytics.

- **Phase 8 — Processing.** The rule DSL, agent-side execution,
  ingest-side execution, and the tests that prove a rule does the same
  thing in both places.
- **Phase 9 — Routing and sinks.** Multiple destinations, conditional
  routing, per-destination delivery guarantees, and the first three
  sinks: object storage, OTLP, Splunk HEC.
- **Phase 10 — Archive and replay.** The archive format, retention and
  tiering, and replay back into the pipeline or out to a destination.
- **Phase 11 — Fleet.** Config authored centrally, versioned, rolled out
  and observed.

Ordering is deliberate. Processing without routing still pays for itself
by shrinking what is stored; routing without processing forwards
everything and helps nobody. Archive depends on both. Fleet is last
because it manages configuration the earlier phases define — building it
first would mean managing settings that do not exist yet.

## What this does not change

The storage/query split in `/docs/architecture.md`, the licence, the
agent's distro-agnostic constraint, and the Splunk positioning. Cairn OBS
is still a destination first. Everything above is what it takes to also
be the road — and to be honest with anyone who asks why they would run
both.
