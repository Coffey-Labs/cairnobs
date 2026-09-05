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

**Treat that recommendation as settled, because the fleet design already
settled it.** `DesiredOverride` — the only channel that can deliver
anything to an agent — is deliberately a closed, typed shape that cannot
carry arbitrary code, and permanently excludes any field capable of
stranding an agent. Two documents reached the same conclusion from
opposite directions, one reasoning about expressiveness and one about
blast radius. There is no version of this where rules arrive as
JavaScript, because there is no channel that would carry it.

**And one requirement that falls out of the same design, which nothing
has written down until now.** Agent management rests on an invariant it
states explicitly: every editable field "degrades the agent's behavior
without ever cutting off its ability to receive the next correction." A
bad batch size is survivable precisely because the agent still checks in
and can be corrected.

Processing rules break that invariant. A rule that panics, loops
forever, or exhausts memory is not a degraded setting; it is a broken
agent, across however many hosts the rule reached before anyone noticed.

How badly it breaks turns out to depend on something decided for
unrelated reasons: overrides live only in the running process's memory
and are never written to disk, so a restarted agent boots clean and
re-syncs. A fatal rule set therefore produces a crash-loop rather than a
strand, and the agent still checks in on every iteration, so it remains
correctable. That is a much better failure than the stranding an
unfixable `ingest.endpoint` would cause — and it is load-bearing safety
acquired by accident, which means persisting overrides to disk later
would silently convert every crash-loop into a strand.

Phase 8 still owes a deliberate answer here rather than a discovered
one. [`phase-8-processing-design.md`](phase-8-processing-design.md)
proposes total evaluation as the primary guarantee — a rule set that
provably cannot panic, loop unboundedly or allocate without limit, which
a typed declarative DSL can offer and which is a third argument for one
— with apply-then-verify as a backstop.

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
precisely because you can get it back.

Retention is half of a question rather than all of one. `api/logretention`
already ships operator-driven preview and delete, with an owner-only
per-agent floor. What does not exist is an *automatic* TTL policy, which
is the half `/docs/architecture.md` still lists as unresolved, and the
half that stops being deferrable here — tiering to object storage and
reading back from it is the same question asked from the other side.

### 4. Fleet management — already built

This section used to say that config flowed to the agent from the host
rather than from the platform, and that the direction of travel was what
was missing. That has not been true for some time.
[`agent-management-design.md`](agent-management-design.md) records the
whole thing as complete and verified live: config authored centrally
(`PUT /agents/{host}/config`), versioned
(`desired_override_version`/`applied_override_version`), rolled out on the
agent's next check-in, observed on the Agents page, and a restart command
that a real agent picks up and acts on.

What remains is named there and is small: `stop`/`uninstall` lifecycle
commands, true per-host multi-row alerting, and a rule-per-host generator.

The consequence for the roadmap is bigger than the correction. Fleet was
going to be the last phase, on the argument that it manages configuration
the earlier phases define. The mechanism arrived first instead, so the
question is no longer "how do we distribute config" but "what new thing
does Phase 8 need it to carry" — which makes distribution part of Phase 8
rather than a phase of its own.

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

## The third axis: AI that runs on your hardware

Cost is the argument against Splunk. Control is the argument against
Cribl. AI is the third, and it is the one where the difference is not a
feature comparison but a deployment model.

**Plain-English querying is an option today and stays one.** Phase 7
shipped it: ask a question in English, get a structured query back with
an explanation, editable before it runs. It is an alternative to writing
the query, never a replacement for being able to — every generated query
compiles through the same Phase 2 IR and executor as a hand-written one,
with the same tenant scoping, cost guardrails and audit logging. The
model suggests; it does not get a private path to the data.

**AI-assisted analysis and explanation is the end state, and is not
built.** Query authoring answers "how do I ask this". The harder and more
valuable question is "what does this mean" — reading a result set and
saying what changed, explaining why an alert fired and what preceded it,
summarising an incident from the records around it, and pointing at what
to look at next. That is the goal; today only the authoring half exists.

**Local is the non-negotiable part.** The default deployment runs a
self-hosted model through Ollama — `qwen2.5-coder`, Apache-2.0 weights
chosen deliberately so Phase 6's licence work survives contact with the
model. A cloud adapter exists, opt-in and off by default. Nothing leaves
the network to make any of this work.

That is the whole position, and it is worth stating as such rather than
as a feature bullet:

| | Where the model runs | What leaves your network |
|---|---|---|
| Splunk | vendor's cloud | your queries and results |
| Cairn OBS | your hardware, by default | nothing |

Logs are the most sensitive unstructured data most organisations hold —
credentials in stack traces, customer identifiers, internal hostnames and
topology. An assistant that reads them is either running where the data
already is, or it is a data-egress decision wearing a helpful interface.
Anyone who has had to answer that question in a procurement review knows
which of those is easier to sign off.

This also constrains what can be promised. A 7B model on a customer's own
hardware will not match a frontier model on raw capability, and the
honest claim is not that it is as clever — it is that it is good enough
at a bounded task, and that it runs somewhere you control. Analysis
features have to be designed to that budget rather than assuming
somebody's API is one call away.

## Roadmap consequence

Phases 0–7 built the destination. This is a second axis, not a
continuation of the first, and it is worth numbering separately rather
than appending forever to a list that was about analytics.

- **Phase 8 — Processing.** The rule DSL, agent-side execution,
  ingest-side execution, the tests that prove a rule does the same thing
  in both places, and distribution: carrying rule sets through
  `DesiredOverride` without breaking the strand-safety invariant above.
  Drafted in
  [`phase-8-processing-design.md`](phase-8-processing-design.md).
- **Phase 9 — Routing and sinks.** Multiple destinations, conditional
  routing, per-destination delivery guarantees, and the first three
  sinks: object storage, OTLP, Splunk HEC.
- **Phase 10 — Archive and replay.** The archive format, tiering,
  automatic TTL, and replay back into the pipeline or out to a
  destination.

**There is no Phase 11.** It was going to be fleet management, and fleet
management is built — see section 4 above. What is left of it either
belongs to Phase 8 (distributing rules) or is a small named remainder
already tracked in
[`agent-management-design.md`](agent-management-design.md).

Ordering is deliberate. Processing without routing still pays for itself
by shrinking what is stored; routing without processing forwards
everything and helps nobody. Archive depends on both.

The original ordering put fleet last, reasoning that it manages
configuration the earlier phases define. That reasoning was sound and the
world went the other way: the mechanism was built first, and Phase 8 now
inherits a distribution channel rather than needing one built for it
afterwards. Worth recording, because the instinct to schedule the
management layer after the thing it manages is a good one that happened
not to apply here.

## The names already argue the case

Worth writing down because it is useful, not only because it is neat: the
three names describe three different relationships to not knowing where
you are.

**Splunk** is from *spelunking* — the founders have always said so. Caving.
You go down into the dark with a lamp and feel your way along, and what
you find depends on how good you are at feeling around. That is an
honest description of search-driven investigation: powerful in expert
hands, and unforgiving if you do not already know roughly what you are
looking for. Every organisation that has watched its Splunk expertise
walk out of the door with one person knows the shape of that.

**Cribl** is from *cribble* — to sift, from Latin *cribrum*, a sieve. The
same root gives engraving its *manière criblée*, the dotted ground
punched into a plate to make a texture. Both senses land in the same
place: the data is a medium to be worked, sifted, thinned and textured on
its way through. Which is exactly what the product is, and it is a name
about the *material*, not about where anyone is going.

**A cairn** is a stack of stones on open ground. It exists where the path
is not obvious — above the treeline, across moorland, over bare rock —
and it does one job: tell you that someone came this way before, and that
this is the way. No cave, no lamp, no sifting. Daylight, an open trail,
and a marker.

Three properties of a cairn matter here, and each corresponds to
something this project actually does rather than something it merely
claims:

- **It is left by whoever went first, for whoever comes next.** That is
  the runbook culture: every phase carries a document recording what was
  actually run and what it found, including the parts that failed. The
  value is not the stone, it is that somebody bothered.
- **Anyone passing can add to it.** A cairn grows by contribution and
  belongs to nobody. AGPLv3 throughout, no commercial-license wall, and
  an egress path that helps your data leave if you want it to.
- **You can see it from a distance, in daylight.** Nothing about it is a
  dark hole you feel your way along. The query language is legible, the
  AI explains rather than divines, and the plan — including what has not
  been proven — is written down where anyone can read it.

The contrast is not a slogan and should not be turned into one. It is a
reason the positioning holds together: an open trail with markers on it is
a genuinely different proposition to a cave, and to a sieve.

## What this does not change

The storage/query split in `/docs/architecture.md`, the licence, the
agent's distro-agnostic constraint, and the Splunk positioning. Cairn OBS
is still a destination first. Everything above is what it takes to also
be the road — and to be honest with anyone who asks why they would run
both.

Nor does it change the AI goal, which predates this document and outlasts
it: plain-English querying stays an option, AI-assisted analysis and
explanation is where it is going, and both run on a local model by
default. That is not a phase to be finished and ticked off — it is a
property the product keeps, and any pipeline feature above that would
require shipping data to somebody else's model to be useful has answered
the wrong question.
