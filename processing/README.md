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
| `aggregate_count` | `window_ms`, optional `key_fields` |

Rules are evaluated in the order given. Every matching rule's actions
apply, to the record as left by the rule before it.

### Regex: only what both engines support

The agent uses `regex-lite` and ingest uses the full `regex` crate — a
size decision, since the agent ships as a small static musl binary and
the full crate is a megabyte-plus (see
[`phase-8-processing-design.md`](../docs/phase-8-processing-design.md)).

**Cases may only use syntax `regex-lite` accepts.** A case the agent
cannot run is not a conformance case. In practice that means quantifiers,
character classes, alternation and named captures are fine, while
Unicode-aware classes and the richer Perl classes are not.

Both engines are linear-time automata with no catastrophic backtracking,
and both resolve alternation leftmost-first. The second is pinned by a
case rather than trusted, because it is exactly the kind of semantic two
independent implementations can differ on silently.

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

### `aggregate_count` emits a record that never existed

The only action whose output is not the input with edits, so its shape is
worth stating here too. It emits the window's **first record, unchanged**,
plus four attributes:

| attribute | value |
|---|---|
| `cairnobs.aggregated` | `"true"` |
| `cairnobs.count` | records collapsed, as a string |
| `cairnobs.window_start_unix_nano` | first record's timestamp |
| `cairnobs.window_last_unix_nano` | last contributing record's timestamp |

This follows the convention the agent already uses for heartbeat and
host-metrics records: an ordinary record distinguished by a
`cairnobs.*` attribute, rather than a second synthetic-record mechanism.

Three behaviours the cases pin, each of which is easy to get wrong:

- **It tags even when the count is one.** Otherwise `cairnobs.count` is
  present only sometimes, and summing it silently breaks on quiet
  windows.
- **`window_last` is observed, never `window_start + window_ms`.** A
  window flushed early must not claim an end that never happened.
- **A pending window flushes at end of stream.** Record-time windows can
  only be closed by a later record, so without this a burst that stops
  leaves its aggregate unemitted indefinitely. In production the batch
  flush provides the same trigger, which makes `window_ms` a maximum
  rather than a guarantee.

**`stats count` undercounts aggregated data.** The correct idiom is
summing `cairnobs.count`. Whether the query layer should do that
automatically is a Phase 2 question, recorded in
[`phase-8-processing-design.md`](../docs/phase-8-processing-design.md).

## Adding a case

One behaviour per file, named for the behaviour rather than the action.
A case is added for every bug found in either implementation — that is
the mechanism that keeps the two from drifting, and it only works if the
case lands with the fix.
