# Query language reference

Sentry has one query language for everything: filtering, free-text
search, and aggregation, in a single query, against a single endpoint
(`POST /query`), from a single query bar in the web UI or `sentryctl
query` on the command line. You don't pick a "search mode" or a
"reporting mode" first — you write one query, and Sentry figures out
which parts need ClickHouse, which parts need the full-text index, and
combines them.

If you already know Splunk's SPL, most of this will feel immediately
familiar: a base search, piped through a sequence of processing stages.
Sentry's language is a deliberately smaller subset — the operators
people actually use day to day, not SPL's full surface area — plus raw
SQL as an escape hatch for anything the pipe syntax doesn't (yet) cover.

## The shape of a query

```
<base search> | <stage> | <stage> | ...
```

Everything before the first `|` is the base search — a filter and/or a
free-text search. Everything after each `|` is a processing stage that
narrows, reshapes, or summarizes what came before it.

```
service=api | where status>=500 | stats count by host | sort -count
```

Read left to right: start with everything logged by the `api` service,
keep only the entries with `status >= 500`, count how many there are per
`host`, and show the busiest hosts first.

## Filtering

```
field=value
field!=value
field>value
field>=value
field<value
field<=value
```

```
service=api
status>=500
host!=host-03
```

Multiple filters combine with `and` (the default when you don't write a
conjunction at all — see "Combining terms" below):

```
service=api status>=500
service=api and status>=500        (equivalent)
```

## Free-text search

Three ways to search the `message` field's text:

```
timeout                              a single bare word
"connection refused"                 a quoted phrase
message:"connection refused"         the same thing, explicit
```

Free-text search is powered by Sentry's full-text index (Tantivy), which
supports phrase matching and wildcards:

```
message:"exact phrase"
message:"time*"
```

Bare words and quoted phrases can be mixed freely with structured filters
in the same query — that's the whole point of having one language:

```
service=api "connection refused"
message:"connection refused" | stats count by host
```

## Combining terms: `and` / `or`

Adjacent terms with nothing between them are implicitly `and`ed, matching
what most people expect from a search bar:

```
error timeout                        same as: error and timeout
```

`or` works between free-text terms, and Sentry's full-text index handles
it natively:

```
error or timeout
```

**Current limitation:** `or` is not supported between structured filters
(`service=api or service=web` returns a clear error rather than silently
being treated as `and`). If you need this, use two separate queries for
now, or the raw SQL escape hatch. This is a known gap, not an oversight —
full boolean-tree support for structured filters is on the list for a
future release once there's real usage data on how much it's needed.

## Time ranges

```
earliest=-1h                         relative: last hour
earliest=-15m latest=-5m             relative window
earliest="2026-08-14T00:00:00Z"      absolute (RFC 3339)
```

Relative offsets: a number followed by `s` (seconds), `m` (minutes), `h`
(hours), `d` (days), or `w` (weeks), always relative to when the query
runs.

## Pipe stages

### `where` — additional filtering after the base search

Same syntax as the base search's filter terms:

```
service=api | where status>=500
```

### `stats` — aggregation

```
stats count by host
stats count(), avg(latency_ms) as avg_latency by host, service
```

Supported functions: `count`, `sum`, `avg`, `min`, `max`. `count` doesn't
need a field (`count`, `count()`, and `count(*)` are all equivalent);
every other function requires one (`sum(latency_ms)`). Give a result an
explicit name with `as`, or accept the default (the function name, or
`count` for a bare count).

```
stats sum(bytes_sent) as total_bytes by host
```

### `sort` — ordering

```
sort -count           descending by count (the default direction)
sort +host             ascending by host
sort -severity, +host   descending by severity, then ascending by host
```

`-` and `+` mean the same thing they do in most search tools: `-` for
descending, `+` for ascending. No sign at all also means descending.
You can sort by any field from the base data, or by a `stats` result's
column name/alias.

### `fields` — choosing which columns come back

```
fields host, message, severity
```

Without `fields`, you get every column.

### `head` / `tail` — limiting results

```
head        first 100 (the default) results
head 20     first 20
tail 50     last 50, chronologically
```

## Field mapping: what's a "real" column vs. an attribute

Sentry's structured columns are `timestamp`, `host`, `service`,
`severity`, `message`, and `record_id`. Anything else you reference by
name — `status`, `latency_ms`, `winevt.event_id`, whatever your logs
happen to carry — is looked up in the per-record attributes, which are
always stored as text.

This matters for comparisons: `status>=500` only makes sense as a number,
so Sentry casts the attribute's text value to a number for you
automatically when the value you're comparing against looks numeric.
`status="unknown"` compares as text instead, since `"unknown"` isn't a
number. You don't need to do anything differently — this happens based
on what you write on the right-hand side of the comparison — but it's
worth knowing that:

- A field that's missing entirely, or whose value isn't actually numeric,
  reads as `0` in a numeric comparison or aggregation (`toFloat64OrZero`
  semantics) rather than erroring. A typo'd field name will "succeed"
  with everything showing as `0` — if a `stats sum(...)` looks
  suspiciously empty, double-check the field name.
- `stats min()`/`max()` on an attribute field always compares
  numerically, not alphabetically, in this release.
- Querying an attribute is always a little more work for ClickHouse than
  querying a real column — if a field turns out to be central to how you
  query your logs, that's a signal it might be worth promoting to a real
  column in a future schema change (not something you can do yourself
  today).

## Raw SQL

Anything starting with `SELECT` is treated as raw ClickHouse SQL and run
directly, no pipe-syntax parsing involved:

```
SELECT host, count(*) FROM logs WHERE service = 'api' GROUP BY host
```

SELECT-only, single statement — Sentry allowlists this at the API level.
Use this for anything the pipe syntax doesn't cover yet: window
functions, `WITH` clauses, ClickHouse-specific functions, joins across
other tables you've added, and so on. There's no performance penalty for
using SQL over the pipe syntax or vice versa — both compile to the same
execution plan internally.

## Which syntax am I using?

Sentry detects automatically: a query starting with `SELECT` runs as
SQL, anything else runs as the pipe syntax. This covers the overwhelming
majority of real queries with no extra step. If you're writing a pipe
query that happens to start with the literal word "select" as a search
term, set the language explicitly instead of relying on detection:

```json
{"query": "select", "language": "spl"}
```

`language` accepts `"sql"`, `"spl"`, or can be omitted entirely (the
default, auto-detect). The web UI's query bar shows which one it
detected next to the query box, with a dropdown to override it.

## Combining free-text search with aggregation

This is the case that makes Sentry's query language more than "SQL with
extra steps" — free text and aggregation, together, in one query:

```
message:"connection refused" | stats count by host
```

Under the hood: the full-text index resolves which records match the
text search first, then ClickHouse does the counting and grouping over
just those records. You don't need to know this to use it — it's
mentioned here because of the one limitation it implies:

**A single free-text search is capped at 5,000 matching records** when
it's combined with a `stats`/filter stage that needs to know exactly
which records matched (the most-relevant 5,000, not an arbitrary
truncation). A text search alone, with no aggregation, isn't affected by
this cap. If your combined query's text search is broad enough to match
more than 5,000 records, narrow it — a more specific phrase, an added
`where` filter, or a tighter time range — the same way you'd narrow an
overly broad search in any tool.

## Response shape

Every query, regardless of syntax or which backend(s) it touched,
returns the same shape:

```json
{"columns": ["host", "count"], "rows": [["api-01", 42], ["api-02", 17]]}
```

or, on error:

```json
{"error": "a description of what went wrong"}
```

## Quick reference

| Syntax | Meaning |
|---|---|
| `field=value` | equals |
| `field!=value` | not equals |
| `field>value` / `>=` / `<` / `<=` | comparison |
| `"phrase"` / bare word | free-text search on `message` |
| `message:"phrase"` | explicit free-text search |
| `earliest=-1h` / `latest=...` | time range |
| `\| where ...` | additional filter |
| `\| stats count by field` | aggregate |
| `\| sort -field` / `+field` | sort desc / asc |
| `\| fields a, b` | choose columns |
| `\| head N` / `\| tail N` | limit results |
| `SELECT ...` | raw SQL |

## Examples

```
service=api | where status>=500 | stats count by host | sort -count
```
Which hosts are producing the most 5xx errors from the `api` service?

```
message:"connection refused" | stats count by host
```
Where are connection-refused errors coming from?

```
earliest=-24h severity=ERROR | stats count by service | sort -count
```
Error volume by service over the last day.

```
winevt.event_id=4625 | fields host, message | head 20
```
Recent failed Windows logon attempts (a `winevt.*` attribute from the
Windows Event Log source — see `/docs/phase-1-runbook.md`).

```
SELECT host, avg(toFloat64OrZero(attributes['latency_ms'])) AS avg_latency
FROM logs WHERE service = 'api' GROUP BY host ORDER BY avg_latency DESC
```
The same kind of query the pipe syntax's `stats avg(latency_ms) by host`
would produce, written by hand — useful as a starting point if you need
something the pipe syntax doesn't support yet.
