package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sentry/sentry/api/internal/querylang/ir"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing time %q: %v", s, err)
	}
	return tm
}

type fakeSQLRunner struct {
	gotSQL string
	result *Result
	err    error
	calls  int
}

func (f *fakeSQLRunner) RunSQL(_ context.Context, sql string) (*Result, error) {
	f.gotSQL = sql
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &Result{Columns: []string{}, Rows: [][]any{}}, nil
}

type fakeSearchClient struct {
	gotQuery string
	gotLimit uint32
	ids      []string
	err      error
	calls    int
}

func (f *fakeSearchClient) Search(_ context.Context, query string, limit uint32) ([]string, error) {
	f.gotQuery = query
	f.gotLimit = limit
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.ids, nil
}

func TestExecuteRawSQLBypassesEverythingElse(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{}
	plan := &ir.Plan{RawSQL: "SELECT 1"}

	_, err := Execute(context.Background(), plan, sqlRunner, search)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if sqlRunner.gotSQL != "SELECT 1" {
		t.Fatalf("gotSQL = %q, want %q", sqlRunner.gotSQL, "SELECT 1")
	}
	if search.calls != 0 {
		t.Fatalf("expected search not to be called for RawSQL, got %d calls", search.calls)
	}
}

func TestExecutePureClickHousePathSkipsSearch(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{}
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "api"}}}

	_, err := Execute(context.Background(), plan, sqlRunner, search)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if search.calls != 0 {
		t.Fatalf("expected no search calls, got %d", search.calls)
	}
	if !strings.Contains(sqlRunner.gotSQL, "FROM logs") || !strings.Contains(sqlRunner.gotSQL, "`service` = 'api'") {
		t.Fatalf("unexpected SQL: %s", sqlRunner.gotSQL)
	}
}

func TestExecuteTextSearchPrefiltersThenQueriesClickHouse(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{ids: []string{"id-1", "id-2"}}
	plan := &ir.Plan{TextSearch: []ir.TextPredicate{{Query: "connection refused"}}}

	_, err := Execute(context.Background(), plan, sqlRunner, search)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if search.gotQuery != "connection refused" {
		t.Fatalf("search query = %q", search.gotQuery)
	}
	if search.gotLimit != textSearchLimit {
		t.Fatalf("search limit = %d, want %d", search.gotLimit, textSearchLimit)
	}
	if !strings.Contains(sqlRunner.gotSQL, "record_id IN ('id-1','id-2')") {
		t.Fatalf("unexpected SQL: %s", sqlRunner.gotSQL)
	}
}

func TestExecuteTextSearchNoMatchesSkipsClickHouseEntirely(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{ids: nil}
	plan := &ir.Plan{TextSearch: []ir.TextPredicate{{Query: "nothing matches"}}}

	result, err := Execute(context.Background(), plan, sqlRunner, search)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if sqlRunner.calls != 0 {
		t.Fatalf("expected ClickHouse not to be queried when search finds nothing, got %d calls", sqlRunner.calls)
	}
	if len(result.Columns) != 0 || len(result.Rows) != 0 {
		t.Fatalf("expected empty result, got %+v", result)
	}
}

func TestExecuteTextSearchWithAggregation(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{ids: []string{"id-1"}}
	plan := &ir.Plan{
		TextSearch: []ir.TextPredicate{{Query: "connection refused"}},
		Aggregation: &ir.Aggregation{
			Funcs:   []ir.AggFunc{{Func: "count", Alias: "count"}},
			GroupBy: []string{"host"},
		},
	}

	_, err := Execute(context.Background(), plan, sqlRunner, search)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(sqlRunner.gotSQL, "record_id IN ('id-1')") {
		t.Fatalf("expected the text-search prefilter in the WHERE clause: %s", sqlRunner.gotSQL)
	}
	if !strings.Contains(sqlRunner.gotSQL, "GROUP BY `host`") {
		t.Fatalf("expected GROUP BY: %s", sqlRunner.gotSQL)
	}
	if !strings.Contains(sqlRunner.gotSQL, "count() AS `count`") {
		t.Fatalf("expected count() AS `count`: %s", sqlRunner.gotSQL)
	}
}

func TestExecuteSearchErrorPropagates(t *testing.T) {
	sqlRunner := &fakeSQLRunner{}
	search := &fakeSearchClient{err: errors.New("search unavailable")}
	plan := &ir.Plan{TextSearch: []ir.TextPredicate{{Query: "x"}}}

	_, err := Execute(context.Background(), plan, sqlRunner, search)
	if err == nil {
		t.Fatal("expected the search error to propagate")
	}
	if sqlRunner.calls != 0 {
		t.Fatalf("expected ClickHouse not to be queried after a search error, got %d calls", sqlRunner.calls)
	}
}

func TestBuildSQLNumericCastOnAttributesField(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "status", Op: ">=", Value: "500"}}}
	sql := buildSQL(plan, nil)
	want := "toFloat64OrZero(attributes['status']) >= 500"
	if !strings.Contains(sql, want) {
		t.Fatalf("SQL = %q, want it to contain %q", sql, want)
	}
}

func TestBuildSQLStringComparisonOnAttributesField(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "status", Op: "=", Value: "unknown"}}}
	sql := buildSQL(plan, nil)
	want := "attributes['status'] = 'unknown'"
	if !strings.Contains(sql, want) {
		t.Fatalf("SQL = %q, want it to contain %q", sql, want)
	}
}

func TestBuildSQLTopLevelFieldNeverCast(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "123"}}}
	sql := buildSQL(plan, nil)
	if strings.Contains(sql, "toFloat64OrZero") {
		t.Fatalf("top-level field should never be numeric-cast: %s", sql)
	}
	if !strings.Contains(sql, "`service` = '123'") {
		t.Fatalf("unexpected SQL: %s", sql)
	}
}

func TestBuildSQLEscapesInjectionAttemptInValue(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "x'; DROP TABLE logs; --"}}}
	sql := buildSQL(plan, nil)
	// The whole attacker-controlled value must land inside exactly one
	// quoted literal, with its embedded quote backslash-escaped so it
	// can't terminate the literal early -- checking for the escaped
	// form directly, not just the absence of the raw substring (which
	// is a weaker check: "\\'; DROP TABLE" still *contains* "'; DROP
	// TABLE" as a substring, so that alone doesn't prove escaping
	// happened).
	want := `'x\'; DROP TABLE logs; --'`
	if !strings.Contains(sql, want) {
		t.Fatalf("expected the literal %q in SQL, got: %s", want, sql)
	}
}

func TestBuildSQLDefaultLimitAppliedWhenNoneGiven(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "api"}}}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "LIMIT 100") {
		t.Fatalf("expected the default row limit, got: %s", sql)
	}
}

func TestBuildSQLExplicitLimitOverridesDefault(t *testing.T) {
	plan := &ir.Plan{
		Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "api"}},
		Limit:   &ir.Limit{N: 5},
	}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "LIMIT 5") || strings.Contains(sql, "LIMIT 100") {
		t.Fatalf("expected LIMIT 5, got: %s", sql)
	}
}

func TestBuildSQLTailWithoutSortOrdersAscending(t *testing.T) {
	plan := &ir.Plan{Limit: &ir.Limit{N: 10, Tail: true}}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "ORDER BY `timestamp` ASC") {
		t.Fatalf("expected ascending order for tail, got: %s", sql)
	}
}

func TestBuildSQLNoSortDefaultsNewestFirst(t *testing.T) {
	plan := &ir.Plan{}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "ORDER BY `timestamp` DESC") {
		t.Fatalf("expected newest-first default, got: %s", sql)
	}
}

func TestBuildSQLSortByAggregateAlias(t *testing.T) {
	plan := &ir.Plan{
		Aggregation: &ir.Aggregation{
			Funcs:   []ir.AggFunc{{Func: "count", Alias: "count"}},
			GroupBy: []string{"host"},
		},
		Sort: []ir.SortField{{Field: "count", Desc: true}},
	}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "ORDER BY `count` DESC") {
		t.Fatalf("expected ORDER BY on the aggregate alias, got: %s", sql)
	}
}

func TestBuildSQLSortByGroupByField(t *testing.T) {
	plan := &ir.Plan{
		Aggregation: &ir.Aggregation{
			Funcs:   []ir.AggFunc{{Func: "count", Alias: "count"}},
			GroupBy: []string{"host"},
		},
		Sort: []ir.SortField{{Field: "host", Desc: false}},
	}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "ORDER BY `host` ASC") {
		t.Fatalf("expected ORDER BY on the group-by column, got: %s", sql)
	}
}

func TestBuildSQLAggregationOnAttributesFieldAlwaysCasts(t *testing.T) {
	plan := &ir.Plan{
		Aggregation: &ir.Aggregation{
			Funcs: []ir.AggFunc{{Func: "avg", Field: "latency_ms", Alias: "avg_latency"}},
		},
	}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "AVG(toFloat64OrZero(attributes['latency_ms'])) AS `avg_latency`") {
		t.Fatalf("unexpected SQL: %s", sql)
	}
}

func TestBuildSQLProjectionFields(t *testing.T) {
	plan := &ir.Plan{Fields: []string{"host", "message"}}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "SELECT `host` AS `host`, `message` AS `message` FROM logs") {
		t.Fatalf("unexpected SQL: %s", sql)
	}
}

func TestBuildSQLTimeRange(t *testing.T) {
	from := mustParseTime(t, "2026-08-14T00:00:00Z")
	to := mustParseTime(t, "2026-08-14T01:00:00Z")
	plan := &ir.Plan{TimeRange: &ir.TimeRange{From: from, To: to}}
	sql := buildSQL(plan, nil)
	if !strings.Contains(sql, "`timestamp` >= '2026-08-14T00:00:00Z'") {
		t.Fatalf("missing From bound: %s", sql)
	}
	if !strings.Contains(sql, "`timestamp` <= '2026-08-14T01:00:00Z'") {
		t.Fatalf("missing To bound: %s", sql)
	}
}
