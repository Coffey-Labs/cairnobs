// Package ir defines Plan, the intermediate representation both the
// pipe-syntax parser+planner and the raw-SQL passthrough compile down
// to. This is the boundary task 3 asked for: "pipe syntax X compiles to
// IR Y" is testable in planner without any backend; "IR Y executes
// correctly" is testable in executor against fakes, independent of the
// planner. See /docs/query-language-design.md.
package ir

import "time"

type Plan struct {
	// RawSQL, when non-empty, means the entire plan is this opaque
	// ClickHouse SQL string, executed as-is -- every other field below
	// is unused. This is the SQL escape hatch's IR representation: a
	// trivial identity compilation that still flows through the same
	// Plan type and the same executor code path as a parsed pipe query.
	RawSQL string

	// TextSearch predicates route to Tantivy as a prefilter. Empty means
	// no Tantivy involvement at all -- pure ClickHouse.
	TextSearch []TextPredicate

	// Filters are always evaluated in ClickHouse, either directly as
	// WHERE clauses (no TextSearch present) or as an additional filter
	// alongside a Tantivy-sourced record_id IN (...) clause.
	Filters []FilterPredicate

	TimeRange *TimeRange

	// Aggregation is nil for a raw-rows query (no GROUP BY).
	Aggregation *Aggregation

	Sort []SortField

	// Fields is the projection; empty means all columns.
	Fields []string

	Limit *Limit
}

type TextPredicate struct {
	// Query is passed to Tantivy's query parser as-is -- phrase and
	// wildcard syntax already supported there (see /search).
	Query string
}

type FilterPredicate struct {
	Field string
	Op    string // "=", "!=", ">", ">=", "<", "<="
	Value string
}

type Aggregation struct {
	Funcs   []AggFunc
	GroupBy []string
}

type AggFunc struct {
	Func  string // count, sum, avg, min, max
	Field string // empty for count
	Alias string // always set by the planner (defaulted if not given explicitly)
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
	// Absolute bounds -- any relative expression (-1h etc.) is resolved
	// by the planner at compile time, since only it knows "now". A zero
	// time.Time means that bound is unset.
	From time.Time
	To   time.Time
}
