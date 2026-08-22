# Query language design

> **Status:** Design, approved 2026-08-14, not yet implemented (that's
> Task 3). This is the reference Task 3's implementation is built against
> — if implementation reveals this design is wrong somewhere, fix this
> doc in the same change, don't let them drift apart.

## Why this design, in one paragraph

Phases 0–1 shipped two disconnected, placeholder query paths: raw SQL
against ClickHouse, and free-text against Tantivy. Phase 2 needs one
query language that can express both filter/aggregation and free-text
search in a single query, without picking a winner between "give up
structured querying" and "give up full-text search." The approach below
does that by keeping parsing and execution strictly separate (a small
pipe-syntax grammar and an "opaque SQL" passthrough both compile to the
same IR) and by generalizing a mechanism Phase 1 already built and proved
works (Tantivy-prefilter → ClickHouse `IN (...)`) rather than inventing a
new cross-backend join strategy from scratch.

## Grammar

Pipe syntax, SPL-inspired, EBNF-ish:

```
query        := (base_search | pipe_stage) ("|" pipe_stage)*
base_search  := bool_expr                      // implicit filter/search, SPL convention
                                                // omitted entirely when the query starts
                                                // directly with a pipe-stage keyword (e.g.
                                                // `stats count by host`, no leading filter,
                                                // no leading "|") -- means match-everything.
                                                // A field genuinely named "where"/"stats"/etc
                                                // still parses as a filter (`where=foo`),
                                                // disambiguated by comparator lookahead.
pipe_stage   := "where" bool_expr
              | "stats" agg_call ("," agg_call)* ["by" field ("," field)*]
              | "sort" sort_field ("," sort_field)*
              | "fields" field ("," field)*
              | "head" [INT]
              | "tail" [INT]

bool_expr    := term (("and" | "or") term)*
term         := field comparator value          // structured filter -> ClickHouse
              | "earliest" "=" time_expr        // time range lower bound
              | "latest" "=" time_expr          // time range upper bound
              | STRING | QUOTED_STRING          // bare term -> free-text (Tantivy) on `message`
              | "message" ":" QUOTED_STRING     // explicit free-text (Tantivy phrase/wildcard syntax passed through)

comparator   := "=" | "!=" | ">" | ">=" | "<" | "<="
agg_call     := IDENT "(" [field] ")" ["as" IDENT]   // count(), sum(field), avg(field), min(field), max(field)
sort_field   := ["-" | "+"] field                     // "-" = desc (default), "+" = asc
time_expr    := QUOTED_STRING                          // absolute RFC3339
              | "-" INT ("s"|"m"|"h"|"d"|"w")          // relative to query time, e.g. -1h, -7d
field        := IDENT
```

### Worked examples

- `service=api | where status>=500 | stats count by host | sort -count`
  — `service=api` is the base filter (structured, top-level column);
  `where status>=500` filters on `status`, which isn't a top-level
  column (see field mapping below); `stats count by host` aggregates;
  `sort -count` orders descending by the aggregate's implicit `count`
  alias.
- `message:"connection refused" | stats count by host` — free-text
  predicate feeding a ClickHouse aggregation. This is the case task 2
  called "the hardest part" — see Execution below.
- `SELECT host, count(*) FROM logs GROUP BY host` — detected as SQL (see
  Detection below), executed directly against ClickHouse.

## Parser: hand-written recursive descent, no new dependency

This grammar is small and stable — seven pipe-stage kinds, one
expression grammar for filters. A hand-written lexer + recursive-descent
parser beats a combinator library (e.g. `participle`) or a generator
(`goyacc`) here:

- **No new dependency.** Consistent with "ask before adding a new
  external dependency, there's no case for one at this grammar size.
- **Error messages matter** for a user-facing query language in a way
  they don't for most internal parsing — "expected `by` after `stats
  count`, got `sort`" is easy to produce by hand, harder to get right
  through a combinator or generated parser.
- Generator tooling (`goyacc`) adds a codegen build step disproportionate
  to a grammar this size.
- This is the standard approach for small, real query DSLs at this
  scope — not a novel choice.

## The SQL escape hatch: not parsed, wrapped as opaque IR

"Both syntaxes compile to the same IR" does not mean writing a SQL
parser — reimplementing ClickHouse's SQL dialect would be a large,
pointless undertaking when ClickHouse already parses its own SQL. A query
that starts with `SELECT` (case-insensitive — see Detection) skips the
pipe-syntax parser entirely and produces an IR value that wraps the raw
SQL string as an opaque passthrough node. Both syntaxes still flow
through the same `Plan` type and the same executor code path — that's
what "same IR" actually buys (one execution and testing surface), not a
shared abstract syntax tree. The existing SELECT-only / single-statement
/ keyword-blocklist validation (`api/internal/queryapi/validate.go`) is
reused unchanged as the guard before wrapping.

## IR (`Plan`)

```go
type Plan struct {
    RawSQL      string            // set => everything else is ignored; opaque ClickHouse passthrough
    TextSearch  []TextPredicate   // bare terms / message: clauses -> routed to Tantivy
    Filters     []FilterPredicate // structured comparisons -> ClickHouse WHERE
    TimeRange   *TimeRange
    Aggregation *Aggregation      // nil => raw rows, no GROUP BY
    Sort        []SortField
    Fields      []string          // projection; empty => all columns
    Limit       *Limit            // head/tail
}

type TextPredicate struct {
    Query string // passed to Tantivy's query parser as-is
}

type FilterPredicate struct {
    Field string
    Op    string // "=", "!=", ">", ">=", "<", "<="
    Value string
}

type Aggregation struct {
    Funcs   []AggFunc // count/sum/avg/min/max, each with an optional field + alias
    GroupBy []string
}

type AggFunc struct {
    Func  string
    Field string // empty for count()
    Alias string
}

type SortField struct {
    Field string
    Desc  bool
}

type Limit struct {
    N    int
    Tail bool // true = last N (by time), false = first N
}

type TimeRange struct {
    From, To time.Time // relative expressions (-1h etc.) resolved to absolute at compile time
}
```

## Field mapping: top-level columns vs. `attributes`

`logs`' real columns (per `/storage`) are `timestamp, host, service,
severity, message, attributes, record_id`. Any field name in a query
that isn't one of those maps to `attributes['<field>']` — e.g.
`status>=500` compiles to a comparison against `attributes['status']`,
not a top-level column, since `status` isn't promoted (Phase 1's decision
not to promote anything without real usage data still holds). Because
`attributes` is `Map(String,String)`, every stored value is a string;
numeric comparators against a non-top-level field cast via
`toFloat64OrZero(attributes['field'])` when the compared value looks
numeric, otherwise compare as string. This is what makes `where
status>=500` work against the existing schema with no migration —
querying an unpromoted field is always slightly more expensive than a
top-level column, which is worth knowing, not hiding.

## Execution: routing between ClickHouse and Tantivy

The core mechanism already exists and is proven: Phase 1's `/search`
endpoint (`api/internal/queryapi/search.go`, `recordIDsQuery`) already
does exactly steps 1–2 below for text-only queries. Phase 2 generalizes
it into four cases:

1. **No `TextSearch` predicates** → pure ClickHouse path. Build one SQL
   statement directly from `Filters`/`TimeRange`/`Aggregation`/`Sort`/
   `Fields`/`Limit`. The common case, and the fast path.
2. **`TextSearch` predicates, no `Aggregation`** → Phase 1's `/search`
   behavior, generalized: Tantivy resolves matching `record_id`s, then
   `SELECT ... WHERE record_id IN (...)` for the rows, with `Filters`/
   `TimeRange`/`Sort`/`Fields`/`Limit` folded into that same statement.
3. **`TextSearch` predicates *and* `Aggregation`** — the genuinely new
   case (`message:"connection refused" | stats count by host`): Tantivy
   resolves matching `record_id`s as a *prefilter*, not a join, then one
   ClickHouse statement does `WHERE record_id IN (...) AND <Filters>
   GROUP BY <...>`. Aggregation always happens in ClickHouse; Tantivy
   only ever narrows which rows are eligible before that.
4. **`RawSQL` set** → executed as-is against ClickHouse, no Tantivy
   involvement regardless of what the SQL contains. The escape hatch is
   opaque by design — no attempt to detect free-text intent inside raw
   SQL.

### Known scaling limitation

Steps 2/3's `record_id IN (...)` approach breaks down if a text search
matches a large number of rows — the `IN` clause is a literal, quoted
UUID list embedded in the query string. Phase 2's mitigation: cap the
Tantivy prefilter at **5,000** results. Tantivy's `TopDocs` already
returns most-relevant-first, so the cap keeps the *best* matches rather
than an arbitrary truncation, but it's a real limitation on result
completeness for very broad text searches combined with aggregation.
Documented in `/docs/query-language-reference.md`, not silently
swallowed.

This number isn't a first-principles estimate — running the Phase 2
benchmark against a real 1M-row dataset (`/docs/phase-2-runbook.md`)
caught the original 10,000 cap failing outright: 10,000 quoted UUIDs
(~39 bytes each) produces a ~390KB query string, which exceeds
ClickHouse's *default* `max_query_size` (262144 bytes / 256KiB) and
fails with a syntax error rather than degrading gracefully — a much
lower ceiling than "multi-million-entry" suggested before anyone had
actually tried it. 5,000 UUIDs (~195KB) stays safely under that default
with headroom. The real long-term fix (streaming `record_id` batches,
ClickHouse-side text indexing, a different join strategy, or simply
raising `max_query_size` server-side with matching memory sizing) is
explicitly future work, out of scope for Phase 2.

### Post-Phase-2 fix: `earliest=`/`latest=` never actually worked against live ClickHouse

Found during Phase 3's dashboard time-range picker work (the first thing
to run a relative `earliest=`/`latest=` query against real ClickHouse
end-to-end — none of Phase 2's own runbook queries or unit tests
happened to exercise it): `executor/sql.go` formatted `TimeRange` bounds
with `time.RFC3339Nano` (e.g. `2026-08-12T20:17:40.223505479Z`), which
ClickHouse's *implicit* string→`DateTime64` cast for a column-vs-literal
comparison rejects outright — `code: 53, Cannot convert string ... to
type DateTime64(9, 'UTC')`. ClickHouse's implicit cast is strict and
wants `'YYYY-MM-DD HH:MM:SS[.fractional]'` (space-separated, no `T`/`Z`);
the lenient ISO-8601-accepting `parseDateTimeBestEffort` is a different,
explicitly-invoked function, not what a plain `WHERE timestamp >= '...'`
comparison uses. Fixed by `formatClickHouseDateTime64` in `sql.go`. The
Phase 2 unit test that covered this (`TestBuildSQLTimeRange`) only
asserted the generated SQL *string*, against a fake `SQLRunner` — it
never caught this because nothing in that test actually asked real
ClickHouse whether the SQL was valid. Left here as a pointed reminder of
why this project's "actually run it" discipline exists: a passing test
suite and a working feature are not the same claim.

## Where this lives: `api/internal/querylang/`

Not a new top-level component. This subsystem always executes in-process
within `/api` — it doesn't run standalone, doesn't get its own Docker
image, and needs both connections `/api` already holds (the ClickHouse
driver, the search gRPC client). A new top-level directory would imply a
new deployable service, which this isn't.

```
api/internal/querylang/
  lexer/     tokenizer
  ast/       parsed pipe-syntax tree
  parser/    tokens -> ast (recursive descent)
  ir/        Plan and supporting types
  planner/   ast -> Plan (field-mapping rule, SQL-passthrough detection)
  executor/  Plan -> results (the four-case routing above)
```

Mirrors the existing `internal/queryapi`, `internal/searchclient`
convention already in `/api`. Each layer is independently testable per
task 3's requirement: "pipe syntax X compiles to IR Y" tests live in
`parser`/`planner` against fixture ASTs/Plans, no backend needed; "IR Y
executes correctly" tests live in `executor` against fakes for both the
ClickHouse and search-client interfaces, same pattern already used
throughout `/ingest` and `/api`.

## `/query` endpoint: auto-detect, with an explicit override

Detection: a request body's query starting with `SELECT`
(case-insensitive, same rule `validateSelectOnly` already applies) is
SQL; otherwise pipe syntax. Covers the overwhelming common case with no
extra field required. An optional `"language": "sql" | "spl"` field in
the request body overrides detection, for the rare case a pipe query
legitimately starts with the literal word "select" as a bare search
term. Auto-detect-with-override matches the shape of other
inference-with-explicit-override choices already made in this stack
(e.g. severity hints winning over parsed values when present) — good
default ergonomics, no silent ambiguity once a caller cares enough to be
explicit.
