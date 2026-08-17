package parser

import (
	"testing"

	"github.com/sentry/sentry/api/internal/querylang/ast"
)

func TestParseSimpleFilter(t *testing.T) {
	q, err := Parse(`service=api`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q.Base.Terms) != 1 {
		t.Fatalf("expected 1 base term, got %d", len(q.Base.Terms))
	}
	cmp, ok := q.Base.Terms[0].(ast.Comparison)
	if !ok {
		t.Fatalf("expected Comparison, got %T", q.Base.Terms[0])
	}
	if cmp.Field != "service" || cmp.Op != "=" || cmp.Value != "api" {
		t.Fatalf("unexpected comparison: %+v", cmp)
	}
	if len(q.Pipes) != 0 {
		t.Fatalf("expected no pipes, got %d", len(q.Pipes))
	}
}

// TestParseFilterWithHyphenatedValue is a regression test for a real
// bug in the lexer (isIdentPart excluded '-'): this exact query is
// /docs/query-language-reference.md's own canonical unquoted example
// and used to fail with "unexpected MINUS after query".
func TestParseFilterWithHyphenatedValue(t *testing.T) {
	q, err := Parse(`host!=host-03`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	cmp, ok := q.Base.Terms[0].(ast.Comparison)
	if !ok {
		t.Fatalf("expected Comparison, got %T", q.Base.Terms[0])
	}
	if cmp.Field != "host" || cmp.Op != "!=" || cmp.Value != "host-03" {
		t.Fatalf("unexpected comparison: %+v", cmp)
	}
}

// TestParseNegativeTimeExprStillWorks guards against the hyphenated-
// identifier fix above accidentally swallowing the leading sign
// earliest=/latest= depend on -- a leading '-' must stay its own token.
func TestParseNegativeTimeExprStillWorks(t *testing.T) {
	q, err := Parse(`earliest=-1h`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	earliest := q.Base.Terms[0].(ast.TimeBound)
	if earliest.Kind != "earliest" || !earliest.Expr.IsRelative || earliest.Expr.RelativeSign != -1 ||
		earliest.Expr.RelativeN != 1 || earliest.Expr.RelativeUnit != "h" {
		t.Fatalf("unexpected earliest: %+v", earliest)
	}
}

func TestParseFullPipeline(t *testing.T) {
	q, err := Parse(`service=api | where status>=500 | stats count(*) as errors by host | sort -errors`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q.Pipes) != 3 {
		t.Fatalf("expected 3 pipe stages, got %d: %+v", len(q.Pipes), q.Pipes)
	}

	where, ok := q.Pipes[0].(ast.WhereStage)
	if !ok {
		t.Fatalf("stage 0: expected WhereStage, got %T", q.Pipes[0])
	}
	cmp := where.Expr.Terms[0].(ast.Comparison)
	if cmp.Field != "status" || cmp.Op != ">=" || cmp.Value != "500" {
		t.Fatalf("unexpected where comparison: %+v", cmp)
	}

	stats, ok := q.Pipes[1].(ast.StatsStage)
	if !ok {
		t.Fatalf("stage 1: expected StatsStage, got %T", q.Pipes[1])
	}
	if len(stats.Aggs) != 1 || stats.Aggs[0].Func != "count" || stats.Aggs[0].Alias != "errors" {
		t.Fatalf("unexpected stats aggs: %+v", stats.Aggs)
	}
	if len(stats.By) != 1 || stats.By[0] != "host" {
		t.Fatalf("unexpected stats by: %+v", stats.By)
	}

	sort, ok := q.Pipes[2].(ast.SortStage)
	if !ok {
		t.Fatalf("stage 2: expected SortStage, got %T", q.Pipes[2])
	}
	if len(sort.Fields) != 1 || sort.Fields[0].Field != "errors" || !sort.Fields[0].Desc {
		t.Fatalf("unexpected sort fields: %+v", sort.Fields)
	}
}

func TestParseExplicitFreeTextWithAggregation(t *testing.T) {
	q, err := Parse(`message:"connection refused" | stats count by host`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	ft, ok := q.Base.Terms[0].(ast.FreeText)
	if !ok {
		t.Fatalf("expected FreeText, got %T", q.Base.Terms[0])
	}
	if ft.Query != "connection refused" {
		t.Fatalf("Query = %q, want %q", ft.Query, "connection refused")
	}
	stats := q.Pipes[0].(ast.StatsStage)
	if stats.Aggs[0].Func != "count" || stats.Aggs[0].Field != "" {
		t.Fatalf("unexpected agg: %+v", stats.Aggs[0])
	}
}

func TestParseBareWordIsFreeText(t *testing.T) {
	q, err := Parse(`timeout`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	ft, ok := q.Base.Terms[0].(ast.FreeText)
	if !ok || ft.Query != "timeout" {
		t.Fatalf("expected FreeText(timeout), got %+v", q.Base.Terms[0])
	}
}

func TestParseImplicitAndBetweenBareTerms(t *testing.T) {
	q, err := Parse(`error timeout`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q.Base.Terms) != 2 {
		t.Fatalf("expected 2 terms, got %d", len(q.Base.Terms))
	}
	if len(q.Base.Conjs) != 1 || q.Base.Conjs[0] != "and" {
		t.Fatalf("expected implicit 'and', got %+v", q.Base.Conjs)
	}
}

func TestParseExplicitAndOr(t *testing.T) {
	q, err := Parse(`service=api and status=500`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q.Base.Conjs) != 1 || q.Base.Conjs[0] != "and" {
		t.Fatalf("expected explicit 'and', got %+v", q.Base.Conjs)
	}

	q2, err := Parse(`service=api or service=web`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q2.Base.Conjs) != 1 || q2.Base.Conjs[0] != "or" {
		t.Fatalf("expected 'or', got %+v", q2.Base.Conjs)
	}
}

func TestParseTimeBoundsRelativeAndAbsolute(t *testing.T) {
	q, err := Parse(`earliest=-1h latest="2026-08-14T00:00:00Z"`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(q.Base.Terms) != 2 {
		t.Fatalf("expected 2 terms, got %d", len(q.Base.Terms))
	}
	earliest := q.Base.Terms[0].(ast.TimeBound)
	if earliest.Kind != "earliest" || !earliest.Expr.IsRelative || earliest.Expr.RelativeSign != -1 ||
		earliest.Expr.RelativeN != 1 || earliest.Expr.RelativeUnit != "h" {
		t.Fatalf("unexpected earliest: %+v", earliest)
	}
	latest := q.Base.Terms[1].(ast.TimeBound)
	if latest.Kind != "latest" || latest.Expr.Absolute != "2026-08-14T00:00:00Z" {
		t.Fatalf("unexpected latest: %+v", latest)
	}
}

func TestParseFieldsStage(t *testing.T) {
	q, err := Parse(`service=api | fields host, message, severity`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	fields := q.Pipes[0].(ast.FieldsStage)
	want := []string{"host", "message", "severity"}
	if len(fields.Fields) != len(want) {
		t.Fatalf("Fields = %v, want %v", fields.Fields, want)
	}
	for i := range want {
		if fields.Fields[i] != want[i] {
			t.Fatalf("Fields = %v, want %v", fields.Fields, want)
		}
	}
}

func TestParseHeadTailWithAndWithoutN(t *testing.T) {
	q, err := Parse(`service=api | head 10`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	head := q.Pipes[0].(ast.HeadStage)
	if !head.HasN || head.N != 10 {
		t.Fatalf("unexpected head: %+v", head)
	}

	q2, err := Parse(`service=api | tail`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	tail := q2.Pipes[0].(ast.TailStage)
	if tail.HasN {
		t.Fatalf("expected no N, got %+v", tail)
	}
}

func TestParseDottedFieldName(t *testing.T) {
	q, err := Parse(`winevt.event_id=4625`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	cmp := q.Base.Terms[0].(ast.Comparison)
	if cmp.Field != "winevt.event_id" || cmp.Value != "4625" {
		t.Fatalf("unexpected comparison: %+v", cmp)
	}
}

func TestParseSortAscendingWithPlus(t *testing.T) {
	q, err := Parse(`service=api | sort +host`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	sort := q.Pipes[0].(ast.SortStage)
	if sort.Fields[0].Desc {
		t.Fatalf("expected ascending sort, got %+v", sort.Fields[0])
	}
}

func TestParseMultipleSortFields(t *testing.T) {
	q, err := Parse(`service=api | sort -severity, +host`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	sort := q.Pipes[0].(ast.SortStage)
	if len(sort.Fields) != 2 {
		t.Fatalf("expected 2 sort fields, got %d", len(sort.Fields))
	}
	if sort.Fields[0].Field != "severity" || !sort.Fields[0].Desc {
		t.Fatalf("unexpected first sort field: %+v", sort.Fields[0])
	}
	if sort.Fields[1].Field != "host" || sort.Fields[1].Desc {
		t.Fatalf("unexpected second sort field: %+v", sort.Fields[1])
	}
}

func TestParseMultipleAggregations(t *testing.T) {
	q, err := Parse(`service=api | stats count() as n, avg(latency_ms) as avg_latency by host`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	stats := q.Pipes[0].(ast.StatsStage)
	if len(stats.Aggs) != 2 {
		t.Fatalf("expected 2 aggs, got %d: %+v", len(stats.Aggs), stats.Aggs)
	}
	if stats.Aggs[1].Func != "avg" || stats.Aggs[1].Field != "latency_ms" || stats.Aggs[1].Alias != "avg_latency" {
		t.Fatalf("unexpected second agg: %+v", stats.Aggs[1])
	}
}

// --- error cases ---

func TestParseErrorEmptyQuery(t *testing.T) {
	if _, err := Parse(``); err == nil {
		t.Fatal("expected an error for an empty query")
	}
}

func TestParseErrorUnknownPipeStage(t *testing.T) {
	if _, err := Parse(`service=api | bogus`); err == nil {
		t.Fatal("expected an error for an unknown pipe stage")
	}
}

func TestParseErrorMissingComparatorValue(t *testing.T) {
	if _, err := Parse(`service=`); err == nil {
		t.Fatal("expected an error for a missing comparison value")
	}
}

func TestParseErrorUnknownAggFunc(t *testing.T) {
	if _, err := Parse(`service=api | stats median(latency)`); err == nil {
		t.Fatal("expected an error for an unknown aggregation function")
	}
}

func TestParseErrorInvalidTimeUnit(t *testing.T) {
	if _, err := Parse(`earliest=-1x`); err == nil {
		t.Fatal("expected an error for an invalid time unit")
	}
}

func TestParseErrorTrailingGarbage(t *testing.T) {
	if _, err := Parse(`service=api extra ) tokens`); err == nil {
		t.Fatal("expected an error for trailing unparseable tokens")
	}
}

func TestParseErrorUnterminatedStringInFreeText(t *testing.T) {
	if _, err := Parse(`message:"unterminated`); err == nil {
		t.Fatal("expected an error for an unterminated quoted string")
	}
}
