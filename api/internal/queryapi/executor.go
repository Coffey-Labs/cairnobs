package queryapi

import (
	"context"
	"fmt"
	"reflect"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type QueryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Executor runs arbitrary (pre-validated) SELECT statements against
// ClickHouse and shapes the result into JSON-friendly columns/rows,
// discovering the result's column set at query time via reflection since
// the query itself is arbitrary.
type Executor struct {
	conn driver.Conn
}

func NewExecutor(conn driver.Conn) *Executor {
	return &Executor{conn: conn}
}

func (e *Executor) Execute(ctx context.Context, sql string) (*QueryResult, error) {
	rows, err := e.conn.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()

	columnTypes := rows.ColumnTypes()
	result := &QueryResult{
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
