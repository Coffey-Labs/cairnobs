package executor

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sentry/sentry/api/internal/querylang/ir"
)

// defaultRowLimit is the safety net when a raw-row query has neither an
// explicit head/tail nor an aggregation -- without it, a bare `service=api`
// with no other pipe stages would return every matching row unbounded.
// Independent of planner's own defaultLimit (same value, different
// concern: that one fills in `head`/`tail` with no N given; this one
// guards queries that never mention head/tail at all).
const defaultRowLimit = 100

// logs' real columns, per /storage. Anything else maps to
// attributes['field'] -- see /docs/query-language-design.md's "Field
// mapping" section.
var topLevelFields = map[string]bool{
	"timestamp": true,
	"host":      true,
	"service":   true,
	"severity":  true,
	"message":   true,
	"record_id": true,
}

func buildSQL(plan *ir.Plan, recordIDFilter []string) string {
	var sb strings.Builder

	sb.WriteString("SELECT ")
	sb.WriteString(selectClause(plan))
	sb.WriteString(" FROM logs")

	if where := buildWhereClause(plan, recordIDFilter); where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(where)
	}

	if plan.Aggregation != nil && len(plan.Aggregation.GroupBy) > 0 {
		sb.WriteString(" GROUP BY ")
		cols := make([]string, len(plan.Aggregation.GroupBy))
		for i, g := range plan.Aggregation.GroupBy {
			cols[i] = columnExpr(g)
		}
		sb.WriteString(strings.Join(cols, ", "))
	}

	writeOrderBy(&sb, plan)

	if plan.Limit != nil {
		fmt.Fprintf(&sb, " LIMIT %d", plan.Limit.N)
	} else if plan.Aggregation == nil {
		fmt.Fprintf(&sb, " LIMIT %d", defaultRowLimit)
	}

	return sb.String()
}

func writeOrderBy(sb *strings.Builder, plan *ir.Plan) {
	switch {
	case len(plan.Sort) > 0:
		sb.WriteString(" ORDER BY ")
		parts := make([]string, len(plan.Sort))
		for i, s := range plan.Sort {
			dir := "ASC"
			if s.Desc {
				dir = "DESC"
			}
			parts[i] = sortColumnExpr(plan, s.Field) + " " + dir
		}
		sb.WriteString(strings.Join(parts, ", "))
	case plan.Limit != nil && plan.Limit.Tail:
		// `tail N` with no explicit sort: order ascending so LIMIT N
		// takes the chronologically *last* N rows. Callers wanting
		// strict newest-first display order re-sort client-side --
		// documented in the query language reference.
		sb.WriteString(" ORDER BY `timestamp` ASC")
	case plan.Aggregation == nil:
		// Raw-row queries with no explicit sort default to newest-first,
		// matching the Phase 0/1 UI default.
		sb.WriteString(" ORDER BY `timestamp` DESC")
	}
}

// sortColumnExpr resolves a sort field against an aggregation's own
// output columns (alias or group-by field) before falling back to the
// normal top-level/attributes mapping -- `sort -count` after `stats
// count` refers to the aggregate's alias, not a raw column.
func sortColumnExpr(plan *ir.Plan, field string) string {
	if plan.Aggregation != nil {
		for _, f := range plan.Aggregation.Funcs {
			if f.Alias == field {
				return quoteIdent(field)
			}
		}
		for _, g := range plan.Aggregation.GroupBy {
			if g == field {
				return columnExpr(field)
			}
		}
	}
	return columnExpr(field)
}

func selectClause(plan *ir.Plan) string {
	if plan.Aggregation != nil {
		parts := make([]string, 0, len(plan.Aggregation.GroupBy)+len(plan.Aggregation.Funcs))
		for _, g := range plan.Aggregation.GroupBy {
			parts = append(parts, columnExpr(g)+" AS "+quoteIdent(g))
		}
		for _, f := range plan.Aggregation.Funcs {
			parts = append(parts, aggExpr(f)+" AS "+quoteIdent(f.Alias))
		}
		return strings.Join(parts, ", ")
	}
	if len(plan.Fields) > 0 {
		parts := make([]string, len(plan.Fields))
		for i, f := range plan.Fields {
			parts[i] = columnExpr(f) + " AS " + quoteIdent(f)
		}
		return strings.Join(parts, ", ")
	}
	return "*"
}

// aggExpr always numeric-casts non-top-level (attributes-map) fields for
// sum/avg/min/max, unlike comparison predicates where casting is
// conditional on whether the compared value looks numeric -- an
// aggregate function is inherently a numeric (or, for min/max,
// order-comparable) operation, so there's no "maybe string" case the way
// there is for `field=value`. Known Phase 2 limitation: min/max on a
// non-top-level field always compares numerically, not lexicographically
// -- string min/max on attributes isn't supported this phase.
func aggExpr(f ir.AggFunc) string {
	if f.Func == "count" {
		return "count()"
	}
	col := columnExpr(f.Field)
	if !topLevelFields[f.Field] {
		col = "toFloat64OrZero(" + col + ")"
	}
	return strings.ToUpper(f.Func) + "(" + col + ")"
}

func buildWhereClause(plan *ir.Plan, recordIDFilter []string) string {
	var conds []string

	if len(recordIDFilter) > 0 {
		quoted := make([]string, len(recordIDFilter))
		for i, id := range recordIDFilter {
			quoted[i] = quoteLiteral(id)
		}
		conds = append(conds, "record_id IN ("+strings.Join(quoted, ",")+")")
	}

	for _, f := range plan.Filters {
		conds = append(conds, buildComparisonSQL(f))
	}

	if plan.TimeRange != nil {
		if !plan.TimeRange.From.IsZero() {
			conds = append(conds, "`timestamp` >= "+quoteLiteral(formatClickHouseDateTime64(plan.TimeRange.From)))
		}
		if !plan.TimeRange.To.IsZero() {
			conds = append(conds, "`timestamp` <= "+quoteLiteral(formatClickHouseDateTime64(plan.TimeRange.To)))
		}
	}

	return strings.Join(conds, " AND ")
}

// formatClickHouseDateTime64 formats t the way ClickHouse's implicit
// string->DateTime64 CAST expects for a WHERE-clause comparison:
// "YYYY-MM-DD HH:MM:SS[.fractional]", space-separated, no 'T'/'Z'. This
// is a real, measured requirement, not a guess: an ISO-8601/RFC3339Nano
// literal (e.g. "2026-08-12T20:17:40.223505479Z", what time.RFC3339Nano
// produces) fails at query time with "code: 53, Cannot convert string
// ... to type DateTime64(9, 'UTC')" -- ClickHouse's *implicit* cast used
// for column-vs-literal comparisons is strict, unlike the lenient
// parseDateTimeBestEffort used elsewhere in ClickHouse. Found by
// actually running a dashboard panel with a relative earliest= against
// live ClickHouse (Phase 2's own unit tests never caught this: they
// assert against a fake SQLRunner that checks the generated SQL string,
// not that ClickHouse accepts it, and none of Phase 2's own live-stack
// runbook queries happened to use earliest=/latest= at all).
func formatClickHouseDateTime64(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999999")
}

// buildComparisonSQL numeric-casts a non-top-level field only when the
// compared value itself looks numeric -- `status>=500` casts (numeric
// comparison intent), `status="unknown"` doesn't (string comparison
// intent). Top-level fields are never cast; ClickHouse compares them
// against a string literal natively (LowCardinality(String)/String
// compare as-is; DateTime64 columns need formatClickHouseDateTime64's
// exact literal shape, handled in buildWhereClause above, not here).
func buildComparisonSQL(f ir.FilterPredicate) string {
	if !topLevelFields[f.Field] && isNumericLiteral(f.Value) {
		return "toFloat64OrZero(" + columnExpr(f.Field) + ") " + f.Op + " " + f.Value
	}
	return columnExpr(f.Field) + " " + f.Op + " " + quoteLiteral(f.Value)
}

func columnExpr(field string) string {
	if topLevelFields[field] {
		return quoteIdent(field)
	}
	return "attributes[" + quoteLiteral(field) + "]"
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteLiteral is the actual injection defense for every user-controlled
// string embedded in generated SQL (filter values, attribute keys, time
// bounds, record_ids). Field/keyword tokens from the lexer are already
// constrained to [a-zA-Z0-9_.] by construction (see lexer.isIdentPart)
// and can't carry SQL metacharacters at all, but quoted-string *values*
// can contain anything, so this can't be skipped for them.
func quoteLiteral(s string) string {
	var sb strings.Builder
	sb.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '\'':
			sb.WriteString(`\'`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('\'')
	return sb.String()
}

var numericLiteralRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

func isNumericLiteral(s string) bool {
	return numericLiteralRe.MatchString(s)
}
