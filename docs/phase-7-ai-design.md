# Phase 7 AI design: model provider architecture

Task 2 deliverable — the model-provider abstraction, presented for
review before any implementation is built against it, per the phase
brief's explicit stop point. Nothing past the interface itself
(`api/ai/provider/provider.go`) has been built yet.

## Provider interface

`api/ai/provider/provider.go` (written, compiles, not yet consumed by
anything). Four operations — `Translate`, `Complete`, `Explain`, `Fix` —
one `Provider` interface, same narrow-interface-plus-fake pattern
`querylang/executor`'s `SQLRunner`/`SearchClient` already established in
this codebase, so a production implementation and a test fake both
satisfy the same small surface.

Design choices worth calling out explicitly:

- **Every result that produces a query returns text, never executes
  anything.** The package has no dependency on `executor` or `planner`
  at all — a `TranslateResult`/`FixResult`'s query text is handed back
  to the caller, which is responsible for running it through the
  unchanged `planner.Compile` → (new) cost guard → `executor.Execute`
  pipeline. This is the mechanical enforcement of the phase's
  non-negotiable principle: there is no code path by which a
  `Provider` implementation could execute a query itself, because the
  interface doesn't give it the means to.
- **`Confidence` is a three-value enum (`high`/`medium`/`low`), not a raw
  float.** A model's self-reported numeric confidence isn't a calibrated
  probability; treating it as one (thresholding at some specific float)
  would be false precision. Three bands are enough to drive real UI
  behavior — task 10 wants low confidence stated plainly, and a `Low`
  band with a required `LowConfidenceReason` is how that's enforced at
  the type level rather than left to prompt-following.
- **`Complete` returns a suggestion, not a full requery.** Ghost-text
  needs exactly the continuation to render after the cursor; making the
  caller diff the model's output against its own input to find the new
  part would be fragile and unnecessary.
- **`Explain` is reused for Track B's "explain the translation," not
  duplicated.** `ExplainRequest.OriginalIntent` is optional — empty for
  Track A's "explain this query I wrote" affordance, populated when
  Track B calls it right after `Translate` to describe *how the NL
  became this query* rather than only describing the query in
  isolation. One operation, two contexts, matching task 10's explicit
  instruction not to build a second explanation mechanism.
- **`Fix` never has a silent-apply path.** `FixResult` is a suggestion +
  explanation; the diff rendering and accept/dismiss decision belong to
  the caller (the UI), matching task 7's explicit requirement.
- **No streaming in this interface, deliberately.** A streamed
  token-by-token response would help perceived latency for `Complete`
  especially, but adds real complexity (partial-JSON handling,
  cancellation semantics) this design doesn't take on in v1. Flagged as
  a candidate follow-up if `Complete`'s latency budget (task 5) turns
  out not to be met by a small model's normal non-streamed response
  time — not preemptively built.

## Primary provider: Ollama, not vLLM

| | Ollama | vLLM |
|---|---|---|
| Hardware floor | Runs on CPU (slow) or a single consumer GPU via quantized (GGUF) models | Needs a real GPU; not practically CPU-viable |
| Deployment complexity | Single binary/container, `ollama pull <model>`, built-in REST API | Python server, CUDA/driver management, more moving parts |
| Throughput under concurrency | Adequate for one-user-at-a-time interactive use; not built for high concurrent QPS | Purpose-built for high-throughput serving (continuous batching, PagedAttention) |
| Fit for this project | Matches `docker-compose for local/homelab` (PROJECT-SPEC.md's stated deployment target) — most self-hosters won't have a dedicated inference GPU | Fits a provisioned-GPU SaaS inference tier — not this phase's target (cloud is the opt-in secondary path, not primary) |

Cairn OBS's actual AI workload shape is one interactive query bar per user
at a time, not a high-QPS inference-serving problem — vLLM's real
advantage (batched throughput at scale) isn't the bottleneck this phase
has. Ollama's lower hardware floor and much simpler operational story
directly serve the "self-hostable, homelab-friendly default" requirement
task 2 sets. **Decision: Ollama**, reachable over its REST API
(`http://localhost:11434` by default), matching every other
external-service integration in this codebase (network boundary, not a
linked library).

## Model recommendation

Selection criteria per the brief: license (this project just finished an
entire phase auditing for OSI-approved-only licenses — recommending a
model under a restricted custom license here would directly contradict
that), hardware footprint (must have a genuinely homelab-viable size,
not just a large flagship variant), and code/structured-output quality
(pipe-syntax generation is closer to code/DSL generation than prose).

Ruled out, with reasons, rather than silently skipped:
- **Llama 3.x** (Meta): strong quality, but Meta's Llama Community
  License is a custom license with a usage restriction (a >700M MAU
  clause) — not OSI-approved open source. Inconsistent with this
  project's own just-completed license posture.
- **Gemma 2/CodeGemma** (Google): same shape of problem — a custom
  license with usage restrictions, not pure Apache/MIT.
- **DeepSeek-Coder-V2**: strong at code, but its license also carries
  use restrictions beyond a standard permissive grant.
- **StarCoder2** (BigCode): OpenRAIL-M is a "responsible AI license"
  with behavioral-use restrictions — again not a clean permissive grant.

**Recommended: the Qwen2.5-Coder family (Alibaba), Apache-2.0 licensed**
— genuinely OSI-approved, no usage restrictions, strong benchmarked
performance on code/SQL/structured-output generation specifically (not
just general chat), and available in a real size range so the hardware
floor is a deployment choice, not a fixed cost:

| Model | Approx. footprint (4-bit quantized via Ollama) | Suggested role |
|---|---|---|
| `qwen2.5-coder:1.5b` | ~1-2GB RAM/VRAM, CPU-viable | The fast/small end of task 2's per-operation config (see below) — candidate for `Complete`'s tight latency budget |
| `qwen2.5-coder:7b` | ~5-6GB VRAM recommended, CPU possible but slow | **Default recommendation** for `Translate`/`Explain`/`Fix` — the balance point between quality and a realistic self-host minimum spec |
| `qwen2.5-coder:14b` / `32b` | ~10-20GB+ VRAM | Optional upsell for deployments with more GPU headroom wanting better translation quality; not the default |

**Proposed default deployment**: `qwen2.5-coder:7b` for every operation
unless per-operation config (below) is explicitly set otherwise. This is
the one item the brief says needs your confirmation before finalizing,
since it sets the minimum hardware bar every self-hosting operator reads
as "what do I need to run this."

## Secondary provider: cloud adapter

A single, vendor-neutral adapter implementing `Provider` against an
OpenAI-compatible chat-completions HTTP API (covers OpenAI itself and
the several other providers — including some serving open-weight models
— that expose the same wire contract), rather than a bespoke adapter per
vendor. Concretely:

- **Off by default, opt-in per-tenant.** Enablement is a tenant-level
  setting (Phase 4's tenant/org model, `enterprise/`'s config surface —
  the same "core defines the interface, enterprise/ owns tenant-scoped
  policy over a network call" shape `api/authz.HTTPAuthorizer` already
  uses), not a global deployment flag. A single-tenant deployment with
  no `enterprise/` configured never has cloud access available at all,
  matching the "no cloud dependency required for the default deployment"
  exit criterion.
- **Visible warning when enabled.** The settings UI surface that toggles
  this (extending Phase 5's Settings page) shows an explicit,
  un-dismissable-by-default notice that enabling this sends query
  content to a third-party API — not a one-time toast, a persistent
  visual indicator wherever cloud is active, so it isn't forgotten after
  the initial toggle.
- **API key stored server-side only** (`enterprise/`'s existing
  credential-storage conventions — same posture as notification-target
  webhook secrets from Phase 3), never exposed to the browser.

This is architecture, not implementation — no code for this adapter is
built in this task; it's described here so task 3/4 and the tracks can
be designed against a stable shape.

## Per-operation provider/model configuration: building it in now

Decision: **yes, build the routing layer now**, not deferred. Reasoning:

`Complete`'s latency budget is a first-class requirement of task 5
("low enough latency to feel responsive") — the brief itself flags this
as the case where a fast small model matters most. If the interface only
supported one model for every operation, satisfying `Complete`'s latency
target would force *either* a small model for everything (hurting
`Translate`/`Fix` quality) or a large model for everything (breaking
autocomplete's responsiveness) — a real, immediate conflict, not a
hypothetical future one.

What "building it in" actually means, scoped narrowly: a small
config-driven routing table —
`map[Operation]ProviderConfig{Provider, Model}` — resolved once at
startup, defaulting every operation to the same provider/model unless a
deployment explicitly overrides one. This is a routing/config concern
sitting *above* the `Provider` interface (a thin dispatcher choosing
which configured `Provider` instance to call per operation), not a
change to the interface itself, and not a general multi-model
orchestration system. A deployment that wants one model for everything
sets one config value and never thinks about this again; a deployment
that wants `qwen2.5-coder:1.5b` for `Complete` and `qwen2.5-coder:7b` for
everything else sets two.

## Schema grounding (task 3)

Built: `api/ai/grounding` (core -- `Service` wraps one `executor.SQLRunner`,
samples ClickHouse via that runner, caches one `provider.SchemaContext`
snapshot, refreshed on an interval) and
`enterprise/internal/groundingregistry` (multi-tenant wiring -- one
cached snapshot per active tenant, all sampled through the same shared
`chrunner.Registry`, which resolves the actual per-tenant ClickHouse
connection from a context stamped via `authz.WithIdentity` -- the same
mechanism `chrunner`'s own doc comment names this exact kind of non-HTTP
caller as being for). Sourced by periodic sampling against `logs`
(service names by frequency, `mapKeys(attributes)` for common attribute
keys, `DISTINCT`-with-a-cap per candidate field for enum-like examples)
-- nothing hand-maintained. Tenant scoping is structural: a grounding
query is just another `RunSQL` call through the exact same tenant-scoped
connection Phase 4 already isolates query execution behind, not a
separate mechanism that could drift out of sync with it.

**Delivery mechanism: embedded in-prompt, not retrieved per-query.**

| | Embedded in-prompt | Retrieved per-query (RAG-style) |
|---|---|---|
| Latency | One model call, no extra round trip | An added retrieval/ranking step before every call |
| Complexity | Grounding data is just serialized into the request | Needs a relevance-ranking step matching partial/NL input against a larger corpus |
| Fit for this project's scale | A tenant's own service/field vocabulary is small (tens of services, capped at 100 attribute keys) -- full embedding doesn't meaningfully bloat the prompt | Solves a problem (a corpus too large to embed) this project doesn't have yet |

Decision: embed the full (capped) `SchemaContext` in every operation's
prompt. The latency cost of an extra retrieval step would hit `Complete`
hardest -- exactly the operation with the tightest budget (task 5) -- for
a problem (prompt bloat from an oversized schema) this project's actual
scale doesn't have. `grounding.go`'s caps (50 services, 100 attribute
keys, 15 of those get real example-value queries, 20 examples max per
field) exist specifically so this stays true even for a tenant with an
unusually sprawling schema. If real usage ever shows a tenant blowing
past these caps in a way that matters, per-query retrieval is the
natural fallback design -- not built now, since nothing today needs it.

## Cost/safety guard (task 4)

**Flagging this as larger than expected, per the brief's own invitation
to do so**: no cost-estimation mechanism existed anywhere in Phase 2/3
before this task. Confirmed by reading the compiler, not assumed --
`ir.Plan.TimeRange` can be entirely unset (both bounds zero), and
nothing between the planner and ClickHouse rejects that; a bare `stats
count by host` with no `earliest=` scans the table's full history today.
Building this from scratch, plus deciding how it applies to *existing*
hand-written queries (not just new AI ones), was real, unplanned design
work beyond "check a number against a threshold."

Built: `api/ai/costguard`, a pure function (`Assess(*ir.Plan) Assessment`)
with three levels (`ok`/`warn`/`reject`) and human-readable reasons:

- **No time bound + aggregation → reject.** An aggregation gets no
  implicit row cap the way a raw-row fetch does -- confirmed by reading
  `executor/sql.go`'s `buildSQL` directly: its `defaultRowLimit=100`
  safety net only applies `else if plan.Aggregation == nil`. An
  unbounded aggregation must scan every matching row regardless of
  output size, with nothing downstream capping that scan.
- **No time bound, no aggregation (raw-row fetch), explicit `Limit` or
  not → warn, not reject, either way.** Real bug caught by this
  package's own tests, not shipped as originally written: an earlier
  version of this rule treated "no explicit `Limit`" as automatically
  worse (reject) than "an explicit `Limit`" (warn) -- wrong, because
  `buildSQL` applies its own `defaultRowLimit=100` to *any* non-aggregation
  query with no explicit `Limit`, so both cases already have the exact
  same real row cap and therefore the same risk level. This is also the
  common "just show me recent logs" pattern the query language's own
  documented default (`head 100`) already treats as normal -- rejecting
  it outright would have flagged a large fraction of legitimate,
  currently-working queries, not just a genuinely dangerous new class.
- **Time range spans over 90 days → warn.**
- **Raw SQL → a best-effort regex check** for a `timestamp` comparison
  anywhere in the text, not a real parse. Explicitly documented as
  lower-confidence than the IR-based checks, which get a structural
  guarantee raw SQL fundamentally can't (same reason Phase 2's raw-SQL
  escape hatch was always opaque to compiler-level enforcement).
- **90-day span and every other numeric threshold here are first-pass
  heuristics**, not benchmarked against a production-scale cluster --
  this environment's own ClickHouse instance holds nowhere near enough
  data to validate them against. Flagged plainly rather than presented
  as tuned.

**How the two callers apply it differently, both wired now:**

- **AI-suggested queries** (Translate/Fix output, and the Optimize
  suggestion, tracks A/B, not yet built): a `reject`-level assessment
  means the suggestion is not offered as a normal accept-and-run action.
  This is the mechanism task 4 asked for -- "reject or flag ... before
  it's ever offered to the user."
- **The existing `/query` handler**: now runs every query (hand-written
  or not) through the same `costguard.Assess` and surfaces the result as
  a new, additive `warnings` field on the response (`queryapi/handler.go`)
  -- never a block. This is a deliberate interpretation of the phase's
  design principle ("the *same* ... cost guardrails as a hand-written
  query"), decided here rather than left ambiguous: hand-written queries
  get the identical assessment an AI-generated one would, so there's
  real parity, but retroactively hard-blocking existing dashboard/
  `cairnobsctl` query patterns that happen to have no time bound is a
  behavioral change this phase didn't set out to make and could break
  real existing usage. `warnings` is `omitempty` -- a client that
  doesn't look for it sees no shape change at all. All existing
  `queryapi` tests still pass unmodified.

## Decisions confirmed 2026-08-16

All four items below were confirmed as proposed, no changes:

1. `qwen2.5-coder:7b` as the default model (Translate/Explain/Fix),
   `qwen2.5-coder:1.5b` as the fast-path option for `Complete`.
2. Ollama over vLLM as the primary inference runtime.
3. The cloud-adapter shape: single OpenAI-compatible adapter, per-tenant
   opt-in, off by default.
4. Per-operation provider/model config built in now, not deferred.

Proceeding to task 3 (schema grounding) and task 4 (cost/safety guard).

## Ollama provider implementation (shared foundation, completion)

The last piece of shared foundation before either track: task 2 designed
`provider.Provider`'s shape, but nothing implemented it until now.
Built, tested, all green:

- `api/ai/provider/ollama` -- a thin `net/http`+`encoding/json` client
  against Ollama's `POST /api/chat`, same shape as
  `alerting/internal/queryclient` (this codebase's existing precedent
  for a small internal HTTP client, no new dependency). Uses Ollama's
  `format: "json"` constrained-output mode for the three operations that
  return structured data (`Translate`/`Complete`/`Fix`); `Explain` asks
  for prose directly since its result is a single string with nothing
  else to parse.
- `prompts.go` -- each operation's system prompt embeds a condensed copy
  of `/docs/query-language-reference.md`'s grammar (kept in sync by
  hand, same as every other place the language is described outside its
  own parser) plus the caller's `SchemaContext`, rendered inline per the
  embedded-in-prompt decision above.
- Handles the common small-model habit of wrapping JSON in a markdown
  code fence despite being told not to (`stripCodeFence`) -- a
  best-effort cleanup, not a guarantee; genuinely malformed output still
  surfaces as a real parse error to the caller rather than being
  silently papered over.
- `Confidence` parsing fails toward `Low` on anything unrecognized, never
  toward assumed correctness -- an empty or garbled confidence field
  from the model is itself a signal something's off.
- `api/ai/router` -- the per-operation dispatch layer task 2 decided to
  build now: a lookup from `Operation` to whichever `provider.Provider`
  was configured for it, falling back to one default. A deployment that
  wants one model for everything configures one; one that wants
  `qwen2.5-coder:1.5b` for `Complete` and `:7b` for everything else
  configures two -- exactly the scope described in task 2's writeup,
  nothing more.

Tested against a real `httptest.Server` standing in for Ollama's actual
wire contract (request shape, JSON-mode response parsing, the
code-fence-stripping fallback, non-200 error surfacing) -- not just
type-checked. **Not yet tested against a real running Ollama server or a
real `qwen2.5-coder` model** in this environment; that's real, disclosed
verification work for `/docs/phase-7-runbook.md` once there's a live
stack to test against, same "written but not run against the live thing"
caveat this project applies throughout. Nothing in `main.go` wires any
of this up yet -- that's deferred to when the actual AI HTTP endpoints
(Track A/B) exist to need it, so there's no dead, unconsumed
configuration sitting in a running binary in the meantime.

All shared foundation (tasks 1-4, plus this provider implementation) is
now built and verified: `go build`/`go vet`/`go test` clean across `api`
and `enterprise`, `hack/check-tenant-boundary.sh` still passes. Per the
CHECKPOINT scope discussion, Track A is next.

## Track A (tasks 5-8): built and live-verified

Backend: `api/ai/aiapi` (`POST /ai/complete`/`explain`/`fix`/`optimize`),
wired into both `api/cmd/api` and `enterprise/cmd/enterprise-api`, gated
on `OLLAMA_BASE_URL` (empty by default -- routes aren't even registered
when unset, matching "no cloud dependency required for the default
deployment" and, by the same reasoning, no forced *local* model
dependency either). `Fix`'s suggested query and `Optimize`'s mechanical
rewrite both run through `costguard.Assess` before being returned --
a `reject`-level assessment sets `blocked: true`, and the frontend
disables the accept action rather than silently offering it.

Frontend: ghost-text completion built directly into
`QueryEditor.svelte` on CodeMirror's own primitives (`StateField` +
`Decoration.widget`, no new dependency), debounced (300ms) and only
triggered when the cursor is at the document end. Explain/Fix/Optimize
added to `QueryBar.svelte` (shared by every consumer -- Search page,
dashboard panel editor, alert rule editor -- though only the Search page
was wired with the full `errorMessage`/`warnings` props in this pass;
the other two get the feature for free whenever they're updated to pass
them, a non-breaking follow-up, not done here).

**Verified live in a real browser**, not just type-checked -- a mock
Ollama server (matching its real `/api/chat` wire contract) run as a
container on the compose network, `api` rebuilt with `OLLAMA_BASE_URL`
pointed at it. All four operations confirmed working end-to-end through
actual UI interaction: Explain's modal, Fix's real diff view with a
genuine parse error and a working Accept that replaced the query bar's
content, Optimize's real cost-guard finding (`severity=ERROR | stats
count by host` against the live seeded ClickHouse data, showing the
actual inline warning and populating the Optimize modal with a real
mechanical rewrite), and ghost-text completion rendering and
accepting correctly on Tab. Graceful degradation confirmed too: with the
mock server stopped, ghost text silently doesn't appear (no error), and
Explain shows a plain "not available right now" message instead of
crashing.

**Two real bugs found and fixed by this live-verification pass** --
neither would have been caught by `svelte-check`/`npm run build`, both
type-correct code:

1. **The CodeMirror view was being destroyed and recreated on every
   keystroke.** `QueryEditor.svelte`'s view-creation `$effect` read
   `value` (needed for the initial `doc:` content), which made it a
   reactive dependent of `value` -- but the editor's own
   `updateListener` writes `value` on every keystroke to keep the
   bindable prop in sync. Every keystroke therefore re-ran the whole
   effect, tearing down and rebuilding the entire `EditorView`. This
   predates Phase 7 (the pattern existed since Phase 5) but never
   manifested as a visible symptom until ghost-text's debounce timer
   gave it something to silently cancel: the effect's cleanup function
   (`clearTimeout(completeTimer); view?.destroy()`) fired moments after
   `scheduleCompletion` set the timer, cancelling it before its 300ms
   elapsed -- `Complete` looked like it was doing nothing, every time.
   Fixed by wrapping the initial `value` read in Svelte 5's `untrack()`,
   so the effect now genuinely only depends on `container` (runs once,
   on mount) -- matching what the component's own second "external
   sync" effect was already documented as assuming.
2. **Ghost text rendered at the start of the query, not after it.** The
   `Decoration.widget` was hardcoded at document position `0` (with a
   comment reasoning that position didn't matter since ghost text is
   only ever shown at the document end) -- true for the field's own
   `null`-vs-suggestion state, but the position still needs to be the
   *current* end of document, not literally position 0. Confirmed
   visually (a screenshot showing the suggestion prepended before typed
   text, not appended after it). Fixed by storing a positioned
   `DecorationSet` directly in the state field, computed inside
   `update()` using `tr.state.doc.length` -- the one place the field's
   `update` function has access to the actual current document.

Both fixes are in the same commit-sized unit as the rest of Track A --
no separate patch, since neither bug shipped anywhere before this pass
caught it.

## Track B (tasks 9-11): built and live-verified

Backend: `POST /ai/translate` in `api/ai/aiapi`, same file and same
patterns as Track A's endpoints. Every translation is compiled
(`planner.Compile`, always `planner.SPL` -- pipe syntax only, per task
9's "narrower, safer surface" choice) and run through `costguard.Assess`
before the response goes out, exactly like `Fix`'s suggested query --
task 9's explicit requirement, not a lesser treatment for a different
track. Three honestly-distinct failure shapes, not collapsed into one:
low confidence (`query` empty, a reason given), a confident answer that
doesn't compile (`compiles: false`, a real and different outcome from
low confidence -- a model can be sure of itself and still wrong about
syntax), and a confident, compiling answer the cost guard blocks.

**Detection mechanism (task 10), decided and documented, not just
implemented**: waiting for a parse error, as the task's own phrasing
suggests, would miss the main case this feature exists for. The pipe
grammar's free-text rule means a plain-English question like "show me
errors from the last day" *parses successfully* -- it becomes a
free-text AND-search for those literal words, not a syntax error, and
silently returns an unhelpful result instead of failing loudly. Built
instead: a cheap client-side heuristic (`looksLikeNaturalLanguage` in
`QueryBar.svelte`) that flags text with none of the pipe syntax's
structural markers (`|`, a comparison operator, `:`) and four or more
words -- long enough that it's very unlikely to be an intentional short
free-text search, which stays untouched. Confirmed live: a real query
like `show me errors from the last day grouped by service` correctly
surfaced the "Interpret as natural language" affordance.

Frontend: a review modal (`QueryBar.svelte`) pre-filled with the current
query bar text (since that's exactly what triggered the detection),
auto-translates on open, shows the generated query in an **editable**
textarea (task 10's explicit "editable inline" requirement) alongside
an auto-fetched explanation -- reusing `Explain` via
`ExplainRequest.OriginalIntent` rather than a separate mechanism, task
10's explicit instruction, already built for Track A's own translation-
review use in the provider interface. "Use this query" inserts into the
query bar and closes the modal; it never runs anything -- the existing
"Run query" button, calling the unchanged `runQuery`/`POST /query`, is
the only confirm-to-run action anywhere in this flow, task 9's
non-negotiable separation. A blocked suggestion the user has since
edited in the textarea is treated as their own text, not the original
flagged one -- re-blocking an edit made specifically to address the
concern would be unhelpful, and `/query`'s own `warnings` field still
assesses whatever they actually end up running regardless.

**Verified live end-to-end**: typed a natural-language-shaped query into
the bar, clicked the affordance, got a real generated query
(`earliest=-1h severity=ERROR | stats count by service | sort -count`)
and a real auto-fetched explanation back from the mock provider, clicked
"Use this query" (confirmed it replaced the query bar content **without
running anything** -- no results table appeared), then manually clicked
"Run query" and confirmed it executed cleanly through the unchanged
`/query` endpoint. Low-confidence and non-compiling-suggestion rendering
were verified via the Go-level handler tests
(`TestHandleTranslateLowConfidenceCarriesReason`,
`TestHandleTranslateNonCompilingQueryIsHonestlyReported`) and code
review rather than a separate live click -- the frontend branch that
renders them is structurally the same conditional-message pattern
already live-verified repeatedly for Explain/Fix/Optimize's own
"unavailable" states, not new untested UI shape.

CLI (task 11): `cairnobsctl query --nl "..."` in `cli/cmd/cairnobsctl/cmd_query.go`.
Same posture as the UI, enforced identically regardless of how the
result was produced: a low-confidence, non-compiling, or cost-guard-blocked
translation is never run, even with `--execute` -- confirmed by
`TestCmdQueryNLBlockedIsNotRunEvenWithExecute` and
`TestCmdQueryNLNonCompilingDoesNotRun`, which fail the test itself if
`/query` is ever called in those cases. Without `--execute`, a real
terminal gets a `y/N` confirmation prompt; a non-interactive invocation
(piped, scripted, CI) prints the translation and exits without running
rather than hanging on a prompt nobody can answer -- detected via
`os.Stdin`'s `ModeCharDevice` bit, no new dependency. `runAndPrintQuery`
is shared between the plain-query path and the post-translation
execute path, so both go through byte-for-byte the same request code
this command already had -- not a parallel implementation.

All three shared-foundation guarantees hold identically for both
tracks, confirmed by inspection of the actual code paths, not asserted:
every generated or suggested query flows through `planner.Compile` (the
unchanged Phase 2 compiler) and `costguard.Assess` before a human ever
sees an offer to run it, and actual execution -- web, CLI, or a
hand-written query -- is always the same `POST /query` handler with the
same tenant-scoped `SQLRunner` and the same audit-logging hook Phase 4
established. No AI code path constructs a `SQLRunner`, calls
`executor.Execute`, or bypasses `authz.RequireRoleOrService` anywhere in
either track.

## Audit logging for AI interactions (task 12): built

Reuses Phase 4's existing `audit_log` table rather than adding a new
one: that table's `detail JSONB` column and extensible `event_type`
CHECK constraint were already designed to carry event shapes other than
"query" (`role_change`/`grant_change`/etc. already skip
`query_text`/`row_count`/`duration_ms` in favor of `detail`) --
`ai_interaction` (`metadata/migrations/0036_add_ai_interaction_event_type.sql`)
is the same shape of extension, not a new table/role/trigger set. Same
append-only, hash-chained, `audit_writer`-role-restricted protections
apply for free.

Scoped to only `translate`/`fix`/`optimize` -- the three flows that
produce a suggestion a user explicitly accepts or dismisses. Deliberately
excludes `complete` (ghost-text fires on every keystroke pause; logging
each one at the same weight as a deliberate review would drown the
signal) and `explain` (produces no suggestion to accept/reject, so the
concept doesn't apply). Both exclusions are named design boundaries, not
oversights.

Chose a single frontend-reported event at the moment of a terminal user
action (accept-and-use or dismiss/cancel) over a two-phase
generation-plus-outcome design correlated by an ID -- simpler, and avoids
threading interaction IDs through every generation response just to
correlate them later.

`api/ai/aiapi.InteractionLogger` is a small interface
(`LogInteraction(ctx, InteractionEntry) error`), nil-by-default on
`Handler` -- same shape as `queryapi.AuditLogger`: a single-tenant
deployment with no `enterprise/` configured simply doesn't log these,
same as it doesn't log query executions today. `POST /ai/log-interaction`
is fail-open, same posture as `queryapi.Handler`'s own audit logging: a
write failure is logged server-side and never surfaced to the user who
just clicked a button.

`enterprise/internal/audit.AIInteractionLogger`
(`ai_interaction_adapter.go`) is the real implementation, mirroring
`QueryAPILogger`'s exact shape: resolves tenant/user identity from ctx
via `authz.IdentityFromContext`, refuses to write an unattributable
entry, and writes through the same `*Store`/pool
`enterprise-api/main.go` already opens for query auditing (one dedicated
`audit_writer`-role pool, reused for both loggers). Operation, input,
output, confidence, and the accepted/edited flags go into `Detail` as
JSON; `FinalQuery` -- the suggested query, whether or not it was
actually used -- goes into the table's existing `QueryText` column,
since that's the one field a security reviewer scanning the audit log
would expect to search on directly.

Frontend: `web/src/lib/api.ts`'s `logInteraction` is fire-and-forget
(`.catch(() => {})` at each call site) -- an audit-write failure, like
every other AI-operation failure in this phase, must never block or
surface an error on the UI action that triggered it. Wired into
`QueryBar.svelte`'s `acceptFix`/`dismissFix`, `acceptOptimize`/
`dismissOptimize`, and `useTranslatedQuery`/`cancelTranslate`. Translate
is the one flow with an edit affordance (the generated-query textarea),
so it's the only one where `edited` can be `true` -- computed by
comparing the textarea's current content against the original suggested
query at the moment of acceptance, not tracked keystroke-by-keystroke.

**Genuinely verified against a live Postgres**, not just unit-tested
against a fake `InteractionLogger`: `metadata/migrations/0036` was
applied to the running dev stack's `cairnobs-metadata-postgres`
(`docker compose up -d --build metadata-migrate`, confirmed via `\d+
audit_log` before/after showing `ai_interaction` added to the
`event_type` CHECK constraint), and two new tests in
`enterprise/internal/audit/integration_test.go` --
`TestAIInteractionLoggerWritesAttributedToContextIdentity` and
`TestAIInteractionLoggerRefusesWithoutIdentity`, the same pattern
`TestQueryAPILoggerWritesAttributedToContextIdentity` already
established -- ran against that real database, through the real
`audit_writer`-role pool, and passed: a real row lands with
`event_type='ai_interaction'`, `query_text` carrying `FinalQuery`, and
`detail` carrying a JSON blob whose `operation`/`accepted` fields
round-trip correctly. This closes the one live-infrastructure gap task
12's backend work would otherwise have shared with Phase 4's own
disclosed "compiles and is unit-tested, never run against a real
database" caveat.

## Integration tests and CI testability (task 13)

Before this task, coverage had a real seam nothing exercised: unit tests
work at two separate layers that never actually touch each other in a
test. `aiapi/handler_test.go`'s `fakeProvider` satisfies
`provider.Provider` directly, bypassing HTTP, JSON, and prompt
construction entirely; `ollama/ollama_test.go` exercises
`ollama.Client`'s wire-format parsing against a stub server, but never
through `aiapi.Handler`'s actual registered routes. Neither proves the
seam between them -- a real `*ollama.Client`, wired through a real
`*router.Router` into a real `*Handler`, reached over real HTTP -- was
ever exercised end to end.

`api/ai/aiapi/integration_test.go` (new) closes that gap: `mockOllamaServer`
stands in for Ollama's real `/api/chat` endpoint (matching its wire
contract byte-for-byte, same technique this phase's live browser
verification used, just returning one fixed canned JSON body per test
instead of one selected by inspecting the prompt), wired into a real
`ollama.New(...)` client, a real `router.New(...)`, and a real
`NewHandler(...)`, then driven by real HTTP requests via `httptest.Server`
against `/ai/translate`, `/ai/fix`, `/ai/complete`, and
`/ai/log-interaction`. `TestIntegrationTranslateBlockedByCostGuard`
specifically proves `costguard.Assess` is actually reached through the
full HTTP stack for an AI-suggested query (an unbounded aggregation
comes back `blocked: true`), not just correct in `costguard_test.go`'s
own isolated unit tests.

**Why this suite is CI-safe and a real model is not**: no live Ollama
server or model weights are needed anywhere in this repo's test suite --
every AI-related test (unit and integration) is deterministic, runs in
milliseconds, and needs no network egress beyond `localhost`. Testing
against a *real* Ollama server running the actual pinned
`qwen2.5-coder:7b` model is deliberately kept **out** of this suite and
out of CI entirely: a multi-gigabyte model download on every run, no
determinism guarantee even at temperature 0 across Ollama/driver
versions, and minutes of inference time per test would make the whole
suite both slow and flaky in a way that erodes trust in CI failures
generally -- the same "boring, well-understood, and fast" bar this
project already holds its dependencies to. The test pyramid this phase
ends up with:

1. **Unit tests** (existing, unchanged by this task): `costguard`,
   `grounding`, `router`, `ollama`'s wire-format parsing, `aiapi`'s
   handler/routing logic via `fakeProvider` -- fast, deterministic, no
   network, all run in CI today.
2. **Integration tests** (this task, new): the mock-Ollama-server suite
   above, plus `cli/cmd/cairnobsctl/cmd_query_test.go`'s existing
   `httptest.Server`-backed coverage of `--nl`/`--execute` (already
   written during Track B, task 11) -- proves the plumbing (HTTP routing,
   JSON contracts, `planner.Compile`/`costguard.Assess` integration,
   audit-log dispatch) without needing real model inference. This is
   what actually runs in CI.
3. **Model-quality verification** (not CI, not automated, disclosed as a
   deliberate gap rather than silently skipped): whether the actual
   pinned model reliably produces valid pipe syntax for realistic
   questions, whether its confidence self-reporting is well-calibrated,
   whether Explain's prose is actually useful -- these are inherently
   non-deterministic, model-quality questions that a wire-contract mock
   cannot answer and that would make CI flaky if it tried. This project's
   established "actually run it" discipline already produced exactly
   this kind of check once (this phase's live browser verification
   against `mock_ollama.py`, and per `/docs/phase-7-runbook.md` once
   written, against a real local Ollama); the recommendation is to keep
   that as a periodic, human-run pre-release checklist item, not a CI
   gate -- the same posture this repo already takes toward the
   ClickHouse/Postgres-backed pieces of Phase 4 that only "compile and
   skip cleanly" in an environment without live infrastructure, rather
   than pretending a flaky or infeasible-to-automate check is covered
   when it isn't.

No new frontend test framework (vitest, Playwright, etc.) was introduced
for this task -- `web/`'s AI-feature verification stays the same live,
manual browser verification already used for Track A/B (see those
sections above), consistent with this project's established frontend
verification discipline rather than adding a new tooling dependency
whose payoff (catching regressions in ghost-text positioning, modal
flows) is already covered by that discipline today.
