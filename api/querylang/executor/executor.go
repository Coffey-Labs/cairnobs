// Package executor runs a compiled ir.Plan and returns results in a
// shape consistent regardless of which backend(s) were hit -- the point
// of compiling to one IR in the first place. See
// /docs/query-language-design.md's "Execution" section for the four
// routing cases implemented here.
package executor

import (
	"context"
	"fmt"

	"github.com/sentry/sentry/api/internal/querylang/ir"
)

type Result struct {
	Columns []string
	Rows    [][]any
}

// SQLRunner executes a raw SQL statement against ClickHouse. *ChRunner
// (chrunner.go) is the production implementation; tests use a fake --
// same narrow-interface pattern used throughout /ingest and /api.
type SQLRunner interface {
	RunSQL(ctx context.Context, sql string) (*Result, error)
}

// SearchClient resolves a Tantivy query into matching record_ids.
type SearchClient interface {
	Search(ctx context.Context, query string, limit uint32) ([]string, error)
}

// textSearchLimit caps how many record_ids a Tantivy prefilter can feed
// into a ClickHouse `IN (...)` clause. See /docs/query-language-design.md's
// "Known scaling limitation" -- this is a real, disclosed limit on result
// completeness for very broad text searches, not an oversight.
//
// 5000, not 10000: confirmed by actually running the Phase 2 benchmark
// (see /docs/phase-2-runbook.md) that 10000 quoted UUIDs (~39 bytes each
// including the comma) produces a ~390KB query string, which exceeds
// ClickHouse's default max_query_size (262144 bytes / 256KiB) and fails
// outright with a syntax error rather than degrading gracefully. 5000
// UUIDs is ~195KB, safely under that default with headroom for the rest
// of the query. This was a real failure caught by running the benchmark,
// not a value chosen from first-principles estimation.
const textSearchLimit = 5000

// Execute runs plan against the given backends. The four cases (per the
// design doc): RawSQL passthrough; pure ClickHouse (no TextSearch); text
// search alone (Tantivy prefilter -> ClickHouse row fetch); text search
// plus aggregation (Tantivy prefilter -> ClickHouse aggregate). Cases 2-4
// share the same buildSQL/buildWhereClause code (sql.go) -- the only
// difference is whether a record_id filter is threaded in.
func Execute(ctx context.Context, plan *ir.Plan, sqlRunner SQLRunner, search SearchClient) (*Result, error) {
	if plan.RawSQL != "" {
		return sqlRunner.RunSQL(ctx, plan.RawSQL)
	}

	var recordIDFilter []string
	if len(plan.TextSearch) > 0 {
		ids, err := search.Search(ctx, plan.TextSearch[0].Query, textSearchLimit)
		if err != nil {
			return nil, fmt.Errorf("full-text search failed: %w", err)
		}
		if len(ids) == 0 {
			return &Result{Columns: []string{}, Rows: [][]any{}}, nil
		}
		recordIDFilter = ids
	}

	sql := buildSQL(plan, recordIDFilter)
	return sqlRunner.RunSQL(ctx, sql)
}
