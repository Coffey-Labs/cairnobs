// Package parser is a hand-written recursive-descent parser for the
// pipe-syntax query language, per /docs/query-language-design.md's
// choice of parser approach (no combinator/generator library -- this
// grammar is small and stable, and hand-written gives full control over
// error messages, which matter for a user-facing query language).
package parser

import (
	"fmt"
	"strconv"

	"github.com/sentry/sentry/api/internal/querylang/ast"
	"github.com/sentry/sentry/api/internal/querylang/lexer"
)

// Parse parses a pipe-syntax query. Callers are responsible for routing
// SQL (queries starting with "SELECT") elsewhere before calling this --
// see planner.Plan and /docs/query-language-design.md's "SQL escape
// hatch" section for why this parser never sees SQL at all.
func Parse(input string) (*ast.Query, error) {
	p := newParser(input)
	return p.parseQuery()
}

type parser struct {
	lex  *lexer.Lexer
	cur  lexer.Token
	next lexer.Token
}

func newParser(input string) *parser {
	p := &parser{lex: lexer.New(input)}
	p.next = p.lex.Next()
	p.advance()
	return p
}

func (p *parser) advance() {
	p.cur = p.next
	p.next = p.lex.Next()
}

func (p *parser) parseQuery() (*ast.Query, error) {
	base, err := p.parseBoolExpr()
	if err != nil {
		return nil, err
	}
	q := &ast.Query{Base: base}
	for p.cur.Kind == lexer.Pipe {
		p.advance()
		stage, err := p.parsePipeStage()
		if err != nil {
			return nil, err
		}
		q.Pipes = append(q.Pipes, stage)
	}
	if p.cur.Kind != lexer.EOF {
		return nil, p.errorf("unexpected %s after query", p.cur)
	}
	return q, nil
}

func (p *parser) parseBoolExpr() (ast.BoolExpr, error) {
	var expr ast.BoolExpr
	term, err := p.parseTerm()
	if err != nil {
		return expr, err
	}
	expr.Terms = append(expr.Terms, term)

	for {
		var conj string
		switch {
		case p.cur.Kind == lexer.Ident && (p.cur.Value == "and" || p.cur.Value == "or"):
			conj = p.cur.Value
			p.advance()
		case p.canStartTerm():
			// Adjacent bare terms with no explicit keyword between them
			// implicitly AND, matching SPL's convention (e.g. `error
			// timeout` means `error AND timeout`).
			conj = "and"
		default:
			return expr, nil
		}
		term, err := p.parseTerm()
		if err != nil {
			return expr, err
		}
		expr.Terms = append(expr.Terms, term)
		expr.Conjs = append(expr.Conjs, conj)
	}
}

func (p *parser) canStartTerm() bool {
	switch p.cur.Kind {
	case lexer.Ident, lexer.String:
		return true
	default:
		return false
	}
}

func (p *parser) parseTerm() (ast.Term, error) {
	switch p.cur.Kind {
	case lexer.Ident:
		if p.cur.Value == "earliest" || p.cur.Value == "latest" {
			return p.parseTimeBound()
		}
		if p.cur.Value == "message" && p.next.Kind == lexer.Colon {
			return p.parseExplicitFreeText()
		}
		if isComparatorStart(p.next.Kind) {
			return p.parseComparison()
		}
		// A bare word with no comparator following it is a free-text
		// search term, not a malformed comparison.
		val := p.cur.Value
		p.advance()
		return ast.FreeText{Query: val}, nil
	case lexer.String:
		val := p.cur.Value
		p.advance()
		return ast.FreeText{Query: val}, nil
	default:
		return nil, p.errorf("expected a filter, comparison, or search term, got %s", p.cur)
	}
}

func (p *parser) parseComparison() (ast.Term, error) {
	field := p.cur.Value
	p.advance()
	op, err := p.parseComparator()
	if err != nil {
		return nil, err
	}
	value, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return ast.Comparison{Field: field, Op: op, Value: value}, nil
}

func (p *parser) parseComparator() (string, error) {
	if !isComparatorStart(p.cur.Kind) {
		return "", p.errorf("expected a comparator (=, !=, >, >=, <, <=), got %s", p.cur)
	}
	op := p.cur.Value
	p.advance()
	return op, nil
}

func isComparatorStart(k lexer.Kind) bool {
	switch k {
	case lexer.Eq, lexer.Neq, lexer.Gt, lexer.Gte, lexer.Lt, lexer.Lte:
		return true
	default:
		return false
	}
}

func (p *parser) parseValue() (string, error) {
	switch p.cur.Kind {
	case lexer.Ident, lexer.String, lexer.Number:
		v := p.cur.Value
		p.advance()
		return v, nil
	default:
		return "", p.errorf("expected a value, got %s", p.cur)
	}
}

func (p *parser) parseTimeBound() (ast.Term, error) {
	kind := p.cur.Value
	p.advance()
	if err := p.expect(lexer.Eq); err != nil {
		return nil, err
	}
	expr, err := p.parseTimeExpr()
	if err != nil {
		return nil, err
	}
	return ast.TimeBound{Kind: kind, Expr: expr}, nil
}

func (p *parser) parseTimeExpr() (ast.TimeExpr, error) {
	if p.cur.Kind == lexer.String {
		v := p.cur.Value
		p.advance()
		return ast.TimeExpr{Absolute: v}, nil
	}

	sign := 1
	switch p.cur.Kind {
	case lexer.Minus:
		sign = -1
		p.advance()
	case lexer.Plus:
		p.advance()
	}

	if p.cur.Kind != lexer.Number {
		return ast.TimeExpr{}, p.errorf("expected a quoted absolute timestamp or a relative offset like -1h, got %s", p.cur)
	}
	n, err := strconv.Atoi(p.cur.Value)
	if err != nil {
		return ast.TimeExpr{}, p.errorf("invalid number %q in time expression", p.cur.Value)
	}
	p.advance()

	if p.cur.Kind != lexer.Ident || !isValidTimeUnit(p.cur.Value) {
		return ast.TimeExpr{}, p.errorf("expected a time unit (s/m/h/d/w) after %d, got %s", n, p.cur)
	}
	unit := p.cur.Value
	p.advance()

	return ast.TimeExpr{IsRelative: true, RelativeSign: sign, RelativeN: n, RelativeUnit: unit}, nil
}

func isValidTimeUnit(u string) bool {
	switch u {
	case "s", "m", "h", "d", "w":
		return true
	default:
		return false
	}
}

func (p *parser) parseExplicitFreeText() (ast.Term, error) {
	p.advance() // "message"
	if err := p.expect(lexer.Colon); err != nil {
		return nil, err
	}
	if p.cur.Kind != lexer.String {
		return nil, p.errorf("expected a quoted string after message:, got %s", p.cur)
	}
	v := p.cur.Value
	p.advance()
	return ast.FreeText{Query: v}, nil
}

func (p *parser) parsePipeStage() (ast.PipeStage, error) {
	if p.cur.Kind != lexer.Ident {
		return nil, p.errorf("expected a pipe stage (where/stats/sort/fields/head/tail), got %s", p.cur)
	}
	switch p.cur.Value {
	case "where":
		p.advance()
		expr, err := p.parseBoolExpr()
		if err != nil {
			return nil, err
		}
		return ast.WhereStage{Expr: expr}, nil
	case "stats":
		return p.parseStatsStage()
	case "sort":
		return p.parseSortStage()
	case "fields":
		return p.parseFieldsStage()
	case "head":
		return p.parseHeadTailStage(false)
	case "tail":
		return p.parseHeadTailStage(true)
	default:
		return nil, p.errorf("unknown pipe stage %q (expected where/stats/sort/fields/head/tail)", p.cur.Value)
	}
}

func (p *parser) parseStatsStage() (ast.PipeStage, error) {
	p.advance() // "stats"
	var stage ast.StatsStage

	agg, err := p.parseAggCall()
	if err != nil {
		return nil, err
	}
	stage.Aggs = append(stage.Aggs, agg)

	for p.cur.Kind == lexer.Comma {
		p.advance()
		agg, err := p.parseAggCall()
		if err != nil {
			return nil, err
		}
		stage.Aggs = append(stage.Aggs, agg)
	}

	if p.cur.Kind == lexer.Ident && p.cur.Value == "by" {
		p.advance()
		field, err := p.parseFieldIdent()
		if err != nil {
			return nil, err
		}
		stage.By = append(stage.By, field)
		for p.cur.Kind == lexer.Comma {
			p.advance()
			field, err := p.parseFieldIdent()
			if err != nil {
				return nil, err
			}
			stage.By = append(stage.By, field)
		}
	}

	return stage, nil
}

func (p *parser) parseAggCall() (ast.AggCall, error) {
	if p.cur.Kind != lexer.Ident {
		return ast.AggCall{}, p.errorf("expected an aggregation function (count/sum/avg/min/max), got %s", p.cur)
	}
	fn := p.cur.Value
	if !isValidAggFunc(fn) {
		return ast.AggCall{}, p.errorf("unknown aggregation function %q (want count/sum/avg/min/max)", fn)
	}
	p.advance()

	// Parens are optional when there's no field: `count`, `count()`, and
	// `count(*)` are all equivalent. `sum(field)` etc. still need them,
	// since that's the only way to name the field.
	var field string
	if p.cur.Kind == lexer.LParen {
		p.advance()
		switch p.cur.Kind {
		case lexer.Ident:
			field = p.cur.Value
			p.advance()
		case lexer.Star:
			p.advance() // count(*) is the same as count()
		}
		if err := p.expect(lexer.RParen); err != nil {
			return ast.AggCall{}, err
		}
	}

	var alias string
	if p.cur.Kind == lexer.Ident && p.cur.Value == "as" {
		p.advance()
		if p.cur.Kind != lexer.Ident {
			return ast.AggCall{}, p.errorf("expected an alias after 'as', got %s", p.cur)
		}
		alias = p.cur.Value
		p.advance()
	}

	return ast.AggCall{Func: fn, Field: field, Alias: alias}, nil
}

func isValidAggFunc(f string) bool {
	switch f {
	case "count", "sum", "avg", "min", "max":
		return true
	default:
		return false
	}
}

func (p *parser) parseSortStage() (ast.PipeStage, error) {
	p.advance() // "sort"
	var stage ast.SortStage

	field, err := p.parseSortField()
	if err != nil {
		return nil, err
	}
	stage.Fields = append(stage.Fields, field)

	for p.cur.Kind == lexer.Comma {
		p.advance()
		field, err := p.parseSortField()
		if err != nil {
			return nil, err
		}
		stage.Fields = append(stage.Fields, field)
	}
	return stage, nil
}

func (p *parser) parseSortField() (ast.SortField, error) {
	desc := true // no explicit sign defaults to descending, same as an explicit "-"
	switch p.cur.Kind {
	case lexer.Minus:
		p.advance()
	case lexer.Plus:
		desc = false
		p.advance()
	}
	if p.cur.Kind != lexer.Ident {
		return ast.SortField{}, p.errorf("expected a field name in sort, got %s", p.cur)
	}
	field := p.cur.Value
	p.advance()
	return ast.SortField{Field: field, Desc: desc}, nil
}

func (p *parser) parseFieldsStage() (ast.PipeStage, error) {
	p.advance() // "fields"
	var stage ast.FieldsStage
	field, err := p.parseFieldIdent()
	if err != nil {
		return nil, err
	}
	stage.Fields = append(stage.Fields, field)
	for p.cur.Kind == lexer.Comma {
		p.advance()
		field, err := p.parseFieldIdent()
		if err != nil {
			return nil, err
		}
		stage.Fields = append(stage.Fields, field)
	}
	return stage, nil
}

func (p *parser) parseFieldIdent() (string, error) {
	if p.cur.Kind != lexer.Ident {
		return "", p.errorf("expected a field name, got %s", p.cur)
	}
	v := p.cur.Value
	p.advance()
	return v, nil
}

func (p *parser) parseHeadTailStage(tail bool) (ast.PipeStage, error) {
	p.advance() // "head" / "tail"
	if p.cur.Kind == lexer.Number {
		n, err := strconv.Atoi(p.cur.Value)
		if err != nil {
			return nil, p.errorf("invalid number %q", p.cur.Value)
		}
		p.advance()
		if tail {
			return ast.TailStage{N: n, HasN: true}, nil
		}
		return ast.HeadStage{N: n, HasN: true}, nil
	}
	if tail {
		return ast.TailStage{}, nil
	}
	return ast.HeadStage{}, nil
}

func (p *parser) expect(k lexer.Kind) error {
	if p.cur.Kind != k {
		return p.errorf("expected %s, got %s", k, p.cur)
	}
	p.advance()
	return nil
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("query syntax error at position %d: %s", p.cur.Pos, fmt.Sprintf(format, args...))
}
