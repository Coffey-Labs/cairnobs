// Package ast defines the parsed pipe-syntax tree. Internal to
// querylang -- not exposed outside it. See
// /docs/query-language-design.md for the grammar this mirrors.
package ast

// Query is `base_search ("|" pipe_stage)*`.
type Query struct {
	Base  BoolExpr
	Pipes []PipeStage
}

// PipeStage is one of WhereStage, StatsStage, SortStage, FieldsStage,
// HeadStage, TailStage.
type PipeStage interface{ isPipeStage() }

type WhereStage struct{ Expr BoolExpr }
type StatsStage struct {
	Aggs []AggCall
	By   []string
}
type SortStage struct{ Fields []SortField }
type FieldsStage struct{ Fields []string }
type HeadStage struct {
	N    int
	HasN bool // false => default limit, decided by the planner
}
type TailStage struct {
	N    int
	HasN bool
}

func (WhereStage) isPipeStage()  {}
func (StatsStage) isPipeStage()  {}
func (SortStage) isPipeStage()   {}
func (FieldsStage) isPipeStage() {}
func (HeadStage) isPipeStage()   {}
func (TailStage) isPipeStage()   {}

// BoolExpr is a sequence of terms joined by "and"/"or". An empty Conjs
// entry between two terms (i.e. no explicit keyword in the source) means
// implicit AND -- SPL's convention for adjacent bare search terms, e.g.
// `error timeout` means `error AND timeout`. Conjs has len(Terms)-1
// elements once Terms has more than one.
type BoolExpr struct {
	Terms []Term
	Conjs []string // "and" | "or", one per gap between consecutive Terms
}

// Term is one of Comparison, TimeBound, FreeText.
type Term interface{ isTerm() }

type Comparison struct {
	Field string
	Op    string // "=", "!=", ">", ">=", "<", "<="
	Value string
}

type TimeBound struct {
	Kind string // "earliest" | "latest"
	Expr TimeExpr
}

type FreeText struct {
	Query string
}

func (Comparison) isTerm() {}
func (TimeBound) isTerm()  {}
func (FreeText) isTerm()   {}

// TimeExpr is either an absolute RFC3339 timestamp or a relative offset
// like -1h/-7d, resolved to an absolute time by the planner (relative to
// compile time), not the parser -- the parser has no notion of "now".
type TimeExpr struct {
	Absolute     string
	IsRelative   bool
	RelativeSign int // -1 or +1
	RelativeN    int
	RelativeUnit string // "s" | "m" | "h" | "d" | "w"
}

type AggCall struct {
	Func  string // count, sum, avg, min, max
	Field string // empty for count()/count(*)
	Alias string // empty => planner assigns a default alias
}

type SortField struct {
	Field string
	Desc  bool
}
