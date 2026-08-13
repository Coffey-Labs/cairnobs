package executor

import (
	"context"
	"fmt"
	"reflect"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ChRunner runs arbitrary (pre-validated) SELECT statements against
// ClickHouse and shapes the result into JSON-friendly columns/rows,
// discovering the result's column set at query time via reflection since
// the query itself is arbitrary. Ported from Phase 0/1's
// api/internal/queryapi.Executor, which this replaces (see task 4) --
// same logic, moved here since it's the query-execution layer's
// plumbing, not specific to the old placeholder /query handler.
type ChRunner struct {
	conn driver.Conn
}

func NewChRunner(conn driver.Conn) *ChRunner {
	return &ChRunner{conn: conn}
}

func (r *ChRunner) RunSQL(ctx context.Context, sql string) (*Result, error) {
	rows, err := r.conn.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()

	columnTypes := rows.ColumnTypes()
	result := &Result{
		Columns: rows.Columns(),
		Rows:    [][]any{},
	}

	for rows.Next() {
		dest := make([]any, len(columnTypes))
		for i, ct := range columnTypes {
			dest[i] = reflect.New(ct.ScanType()).Interface()
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		row := make([]any, len(dest))
		for i, d := range dest {
			row[i] = reflect.ValueOf(d).Elem().Interface()
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return result, nil
}
