// Package planner compiles a query string (either syntax) into ir.Plan.
// This is the single entry point querylang exposes to callers (the /query
// HTTP handler) -- see Compile.
package planner

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/api/internal/querylang/ast"
	"github.com/cairnobs/cairnobs/api/internal/querylang/ir"
	"github.com/cairnobs/cairnobs/api/internal/querylang/parser"
)

// Language selects which syntax a query is written in.
type Language string

const (
	Auto Language = "" // detect from the query text (default)
	SQL  Language = "sql"
	SPL  Language = "spl" // the pipe syntax; named to match the query-language-reference doc
)

const defaultLimit = 100

// Compile turns a query string into a Plan. language overrides
// auto-detection; pass Auto to use the SELECT-prefix heuristic (see
// /docs/query-language-design.md's "Detection" section) -- this exists
// for the rare case a pipe query legitimately starts with the literal
// word "select" as a bare search term.
func Compile(query string, language Language, now time.Time) (*ir.Plan, error) {
	isSQL := language == SQL
	if language == Auto {
		isSQL = looksLikeSQL(query)
	}

	if isSQL {
		if err := validateSelectOnly(query); err != nil {
			return nil, err
		}
		return &ir.Plan{RawSQL: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))}, nil
	}

	q, err := parser.Parse(query)
	if err != nil {
		return nil, err
	}
	return compileQuery(q, now)
}

func looksLikeSQL(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return false
	}
	fields := strings.Fields(trimmed)
	return len(fields) > 0 && strings.EqualFold(fields[0], "SELECT")
}

func compileQuery(q *ast.Query, now time.Time) (*ir.Plan, error) {
	plan := &ir.Plan{}

	textParts, err := compileBoolExpr(q.Base, plan, now)
	if err != nil {
		return nil, err
	}

	for _, stage := range q.Pipes {
		switch s := stage.(type) {
		case ast.WhereStage:
			parts, err := compileBoolExpr(s.Expr, plan, now)
			if err != nil {
				return nil, err
			}
			textParts = append(textParts, parts...)
		case ast.StatsStage:
			agg, err := compileStats(s)
			if err != nil {
				return nil, err
			}
			plan.Aggregation = agg
		case ast.SortStage:
			for _, f := range s.Fields {
				plan.Sort = append(plan.Sort, ir.SortField{Field: f.Field, Desc: f.Desc})
			}
		case ast.FieldsStage:
			plan.Fields = append(plan.Fields, s.Fields...)
		case ast.HeadStage:
			n := defaultLimit
			if s.HasN {
				n = s.N
			}
			plan.Limit = &ir.Limit{N: n, Tail: false}
		case ast.TailStage:
			n := defaultLimit
			if s.HasN {
				n = s.N
			}
			plan.Limit = &ir.Limit{N: n, Tail: true}
		default:
			return nil, fmt.Errorf("internal error: unhandled pipe stage %T", stage)
		}
	}

	if len(textParts) > 0 {
		plan.TextSearch = []ir.TextPredicate{{Query: strings.Join(textParts, " ")}}
	}

	return plan, nil
}

// textPart is one piece of a combined Tantivy query string, tagged with
// the conjunction that precedes it (empty for the first piece).
type textPart struct {
	conj  string // "", "and", "or"
	query string
}

// compileBoolExpr walks one bool_expr (the base search, or a `where`
// stage's expression), populating plan.Filters and plan.TimeRange
// directly, and returning free-text pieces for the caller to fold into
// the combined Tantivy query string.
//
// Scope decision: "or" is only supported between free-text terms, which
// Tantivy's own query parser handles natively once composed into one
// string. "or" between structured comparisons/time-bounds is rejected
// with a clear error rather than silently compiled as "and" -- see
// /docs/query-language-reference.md's limitations section. This keeps
// the executor's generated SQL a flat AND-only WHERE clause, which is
// most of what real queries need; full boolean-tree support for
// structured filters is future work if usage shows it's needed.
func compileBoolExpr(expr ast.BoolExpr, plan *ir.Plan, now time.Time) ([]string, error) {
	var textParts []string

	for i, term := range expr.Terms {
		conj := ""
		if i > 0 {
			conj = expr.Conjs[i-1]
		}

		switch t := term.(type) {
		case ast.Comparison:
			if conj == "or" {
				return nil, fmt.Errorf("query error: \"or\" is not supported between structured filters (%q) in Phase 2 -- only between free-text search terms", t.Field)
			}
			plan.Filters = append(plan.Filters, ir.FilterPredicate{Field: t.Field, Op: t.Op, Value: t.Value})
		case ast.TimeBound:
			if conj == "or" {
				return nil, fmt.Errorf("query error: \"or\" is not supported on time bounds (%s) in Phase 2", t.Kind)
			}
			if err := applyTimeBound(plan, t, now); err != nil {
				return nil, err
			}
		case ast.FreeText:
			q := t.Query
			if strings.ContainsAny(q, " \t") {
				q = `"` + strings.ReplaceAll(q, `"`, `\"`) + `"`
			}
			if conj == "or" {
				textParts = append(textParts, "OR", q)
			} else if len(textParts) > 0 {
				textParts = append(textParts, "AND", q)
			} else {
				textParts = append(textParts, q)
			}
		default:
			return nil, fmt.Errorf("internal error: unhandled term %T", term)
		}
	}

	return textParts, nil
}

func applyTimeBound(plan *ir.Plan, t ast.TimeBound, now time.Time) error {
	when, err := resolveTimeExpr(t.Expr, now)
	if err != nil {
		return err
	}
	if plan.TimeRange == nil {
		plan.TimeRange = &ir.TimeRange{}
	}
	switch t.Kind {
	case "earliest":
		plan.TimeRange.From = when
	case "latest":
		plan.TimeRange.To = when
	}
	return nil
}

func resolveTimeExpr(e ast.TimeExpr, now time.Time) (time.Time, error) {
	if !e.IsRelative {
		t, err := time.Parse(time.RFC3339, e.Absolute)
		if err != nil {
			return time.Time{}, fmt.Errorf("query error: invalid absolute timestamp %q, want RFC3339 (e.g. 2026-08-14T00:00:00Z): %w", e.Absolute, err)
		}
		return t, nil
	}

	var d time.Duration
	switch e.RelativeUnit {
	case "s":
		d = time.Duration(e.RelativeN) * time.Second
	case "m":
		d = time.Duration(e.RelativeN) * time.Minute
	case "h":
		d = time.Duration(e.RelativeN) * time.Hour
	case "d":
		d = time.Duration(e.RelativeN) * 24 * time.Hour
	case "w":
		d = time.Duration(e.RelativeN) * 7 * 24 * time.Hour
	default:
		return time.Time{}, fmt.Errorf("internal error: unknown time unit %q", e.RelativeUnit)
	}
	if e.RelativeSign < 0 {
		d = -d
	}
	return now.Add(d), nil
}

func compileStats(s ast.StatsStage) (*ir.Aggregation, error) {
	agg := &ir.Aggregation{GroupBy: s.By}
	seen := map[string]bool{}
	for _, a := range s.Aggs {
		if a.Func != "count" && a.Field == "" {
			return nil, fmt.Errorf("query error: %s() requires a field, e.g. %s(latency_ms)", a.Func, a.Func)
		}
		alias := a.Alias
		if alias == "" {
			alias = defaultAggAlias(a)
		}
		if seen[alias] && a.Alias == "" {
			// Two unnamed aggs of the same shape would otherwise collide
			// (e.g. `stats sum(a), sum(b)` both defaulting to "sum") --
			// disambiguate by field name.
			alias = alias + "_" + a.Field
		}
		seen[alias] = true
		agg.Funcs = append(agg.Funcs, ir.AggFunc{Func: a.Func, Field: a.Field, Alias: alias})
	}
	return agg, nil
}

func defaultAggAlias(a ast.AggCall) string {
	if a.Func == "count" {
		return "count"
	}
	return a.Func
}

// --- SQL escape hatch validation, ported from the Phase 0/1
// api/internal/queryapi/validate.go guard it replaces (see task 4) ---

// disallowedKeyword is defense-in-depth on top of the SELECT-only gate:
// it catches mutating/administrative statements appearing anywhere in
// the query, not just at the start. Word-boundary matching, not a real
// SQL parser -- same tradeoffs as the Phase 0/1 version this replaces.
var disallowedKeyword = regexp.MustCompile(`(?i)\b(insert|update|delete|alter|drop|truncate|create|grant|revoke|attach|detach|rename|kill|optimize|system|set|exchange|watch)\b`)

// disallowedTableFunction blocks ClickHouse's built-in table functions
// that reach outside ClickHouse itself -- a keyword blocklist for
// mutating statements (above) doesn't touch these at all, since
// `SELECT * FROM url(...)` is a perfectly ordinary read-only SELECT as
// far as validateSelectOnly's other checks are concerned. Every
// function here lets a SELECT-only, RoleViewer-gated query make
// ClickHouse itself issue an outbound request or read a local file on
// the caller's behalf -- cloud-metadata SSRF via url(), a proxy into
// other internal ClickHouse/MySQL/Postgres instances via
// remote()/remoteSecure()/mysql()/postgresql(), and local/object-storage
// file reads via file()/hdfs()/s3()/azureBlobStorage()/deltaLake()/
// iceberg()/hudi(). Same word-boundary-regex tradeoff as
// disallowedKeyword above: this is a blocklist, not a real SQL parser,
// so it can't be the only control -- see the ClickHouse-grant-level
// hardening this should be paired with (table-function usage revoked
// for the role api's raw-SQL path connects as).
var disallowedTableFunction = regexp.MustCompile(`(?i)\b(url|remote|remoteSecure|mysql|postgresql|s3|s3Cluster|hdfs|hdfsCluster|file|odbc|jdbc|executable|cluster|clusterAllReplicas|azureBlobStorage|deltaLake|iceberg|hudi|redis|mongodb)\s*\(`)

func validateSelectOnly(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return fmt.Errorf("query must not be empty")
	}

	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	if trimmed == "" {
		return fmt.Errorf("query must not be empty")
	}
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("only a single statement is allowed")
	}

	firstWord := strings.ToUpper(strings.Fields(trimmed)[0])
	if firstWord != "SELECT" {
		return fmt.Errorf("only SELECT queries are allowed")
	}

	if disallowedKeyword.MatchString(trimmed) {
		return fmt.Errorf("query contains a disallowed keyword")
	}
	if disallowedTableFunction.MatchString(trimmed) {
		return fmt.Errorf("query contains a disallowed table function")
	}

	return nil
}
