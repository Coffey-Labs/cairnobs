# processing

The Phase 8 processing rule language: its specification, and the
conformance suite that *is* that specification.

> **Status:** the suite exists; the implementations do not. Nothing in
> Phase 8 is built. See
> [`/docs/phase-8-processing-design.md`](../docs/phase-8-processing-design.md)
> for the design and its open questions.

## Why this is a top-level directory

Rules run in two places, in two languages: the Rust agent
(`/agent`) and the Go ingest tier (`/ingest`). Neither owns the
definition. This is the same shape as `/proto` — a language-neutral
contract that several modules consume and none of them is the source of
truth for.

## Why the cases are the specification

Two hand-written implementations of one language will diverge. Not
maybe, eventually. The only thing that reliably prevents it is a shared
corpus both must satisfy, written down before either exists.

So the prose below is a summary of `conformance/cases/`, not the other
way round. When they disagree, the cases win, and the prose is the bug.

## Running it

Nothing executes the cases yet, because neither implementation exists.
What runs today is a structural check that every case is well-formed —
valid JSON, known actions, no unknown keys, declared fields that exist:

```sh
python3 processing/conformance/validate.py
```

That is worth having on its own: it stops the corpus rotting between now
and the first implementation, and it fails loudly if someone adds a case
using an action the spec does not define.

Each implementation is expected to add a test that walks
`conformance/cases/*.json`, feeds `inputs` through `rules`, and asserts
the emitted records equal `expect`. Those runners are part of building
each side, not part of this directory.

## Case format

One JSON object per file:

```json
{
  "name": "drop_discards_the_record",
  "description": "A matched drop emits nothing at all.",
  "rules": [
    {
      "match": [{"field": "message", "op": "eq", "value": "noise"}],
      "actions": [{"action": "drop"}]
    }
  ],
  "inputs": [
    {"timestamp_unix_nano": 1000, "host": "h1", "service": "s1",
     "severity": "SEVERITY_INFO", "message": "noise", "attributes": {}}
  ],
  "expect": []
}
```

`inputs` is always a list, even for a single record, because windowed
actions need a sequence. `expect` is the records emitted, in order.

### Records

Mirrors `LogRecord` in `/proto/sentry/logs/v1/logs.proto`, snake_case,
with `severity` as the enum's name string.

`record_id` is deliberately absent from every case. The agent never sets
it, and whether an ingest-side rule can see one depends on where in the
ingest path rules run — an open question. No case may depend on it, so
that decision stays free.

### Matching

A rule matches when **every** clause in `match` holds. An empty `match`
matches every record.

| `op` | Meaning |
|---|---|
| `eq` / `ne` | exact string equality |
| `contains` | substring |
| `prefix` / `suffix` | string boundary |
| `regex` | linear-time regex, unanchored |
| `exists` / `not_exists` | field present (no `value`) |

Addressable fields: `message`, `host`, `service`, `severity`, and
`attributes.<key>`. A field that does not exist compares as absent, not
as empty string — `eq ""` does not match a missing attribute.

### Actions

Applied in order. A `drop` ends processing for that record immediately;
no later action or rule runs.

| Action | Parameters |
|---|---|
| `drop` | — |
| `drop_fields` | `fields` |
| `keep_fields` | `fields` (attributes only; never removes top-level fields) |
| `rename` | `from`, `to` |
| `derive` | `field`, and one of `value` / `from_field` |
| `mask` | `field`, `pattern`, `replacement` |
| `parse_json` | `field` (default `message`), optional `prefix` |
| `parse_regex` | `field`, `pattern` (named captures become attributes) |
| `sample` | `keep_one_in` |
| `suppress_duplicates` | `window_ms`, optional `key_fields` |

Rules are evaluated in the order given. Every matching rule's actions
apply, to the record as left by the rule before it.

### Two determinism decisions the suite forces

Conformance testing cannot assert on nondeterminism, so two things that
are usually left vague are pinned here:

**`sample` is counter-based, not random.** `keep_one_in: 3` keeps the
first record and every third thereafter, per rule, per process. Random
sampling is statistically nicer and untestable; a counter is testable and
close enough at volume.

**Windows are measured on record timestamps, not wall-clock.** A
`suppress_duplicates` window of 5000ms compares
`timestamp_unix_nano` values, so replaying the same input always gives
the same output regardless of how fast the test runs. This also means the
behaviour is correct under backfill, which wall-clock windows are not.

### `aggregate_count` has no cases, deliberately

The design lists it as an action but does not answer what it emits, or
what a query that is not expecting a synthetic record sees. Writing cases
now would invent that answer by accident and freeze it. It stays
unspecified until that question is decided —
[`phase-8-processing-design.md`](../docs/phase-8-processing-design.md)
open question 5.

## Adding a case

One behaviour per file, named for the behaviour rather than the action.
A case is added for every bug found in either implementation — that is
the mechanism that keeps the two from drifting, and it only works if the
case lands with the fix.
