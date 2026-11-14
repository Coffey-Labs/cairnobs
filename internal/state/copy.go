package state

// Copy moves an install off SQLite onto PostgreSQL: `obs-beacond store copy`
// (docs/scale-and-availability.md, Storage). It runs once, with obs-beacond
// stopped, into an empty database. Sealed values go across as they are, so
// the new setup uses the same state key.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// copyTables is every table, parents before the tables that refer to them.
var copyTables = []string{
	"meta", "settings", "users", "passkeys", "setup_links", "sessions",
	"incidents", "incident_notes", "outbox", "audit", "status_log", "host_hours", "workload_hours", "mail_rounds",
	"agents", "agent_certs", "join_tokens",
	"map_edges", "map_services", // what the maps learned: without them every edge starts over as "seen"
	"agent_actions",
}

// CopiedTable is how many rows of a table Copy moved.
type CopiedTable struct {
	Table string
	Rows  int64
}

// Copy copies every row of the SQLite database at from into the PostgreSQL
// database at to, which must be empty. It's all or nothing: one transaction.
func Copy(from, to string, o Options) ([]CopiedTable, error) {
	if IsPostgres(from) {
		return nil, errors.New("copy from: want the SQLite file")
	}
	if !IsPostgres(to) {
		return nil, errors.New("copy to: want a postgres:// URL")
	}
	if _, err := os.Stat(from); err != nil {
		return nil, fmt.Errorf("copy from: %w", err)
	}
	src, err := connect(from, Options{}) // brings it up to this release's schema
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := connect(to, o)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	for _, table := range copyTables {
		var n int64
		if err := dst.queryRow(`SELECT count(*) FROM ` + pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, fmt.Errorf("the PostgreSQL database already has data (%s: %d rows): copy only into an empty one", table, n)
		}
	}
	// Months for all the history there is, so none of it piles up in the
	// default partitions.
	for _, p := range partitioned {
		var lo, hi sql.NullInt64
		if err := src.queryRow(`SELECT MIN(`+p.col+`), MAX(`+p.col+`) FROM `+p.table).Scan(&lo, &hi); err != nil {
			return nil, err
		}
		if lo.Valid {
			if err := dst.EnsurePartitions(time.Unix(lo.Int64, 0), time.Unix(hi.Int64, 0)); err != nil {
				return nil, err
			}
		}
	}

	ctx := context.Background()
	conn, err := dst.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out []CopiedTable
	err = conn.Raw(func(dc any) error {
		pc := dc.(*stdlib.Conn).Conn()
		tx, err := pc.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		for _, table := range copyTables {
			n, err := copyTable(ctx, src, tx, table)
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			out = append(out, CopiedTable{table, n})
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return nil, err
	}
	// Check: the same number of rows on both sides.
	for _, c := range out {
		var n int64
		if err := src.queryRow(`SELECT count(*) FROM ` + c.Table).Scan(&n); err != nil {
			return nil, err
		}
		if n != c.Rows {
			return nil, fmt.Errorf("%s: %d rows in SQLite, %d copied", c.Table, n, c.Rows)
		}
	}
	return out, nil
}

// copyTable streams one table across. Ids PostgreSQL numbers itself (the
// audit log's) aren't copied: rows go in the order SQLite has them, so they
// keep their order.
func copyTable(ctx context.Context, src *DB, tx pgx.Tx, table string) (int64, error) {
	types, err := pgColumns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	rows, err := src.query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return 0, err
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, err
		}
		if table == "audit" && c == "id" {
			continue
		}
		if _, ok := types[c]; !ok {
			rows.Close()
			return 0, fmt.Errorf("column %s isn't in PostgreSQL's schema", c)
		}
		cols = append(cols, c)
	}
	rows.Close()
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
	}
	data, err := src.query(`SELECT ` + strings.Join(quoted, ", ") + ` FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		return 0, err
	}
	defer data.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var bad error
	n, err := tx.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromFunc(func() ([]any, error) {
		if !data.Next() {
			return nil, data.Err()
		}
		if err := data.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]any, len(cols))
		for i, c := range cols {
			v, err := coerce(vals[i], types[c])
			if err != nil {
				bad = fmt.Errorf("column %s: %w", c, err)
				return nil, bad
			}
			row[i] = v
		}
		return row, nil
	}))
	if bad != nil {
		return 0, bad
	}
	return n, err
}

func pgColumns(ctx context.Context, tx pgx.Tx, table string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, t string
		if err := rows.Scan(&c, &t); err != nil {
			return nil, err
		}
		out[c] = t
	}
	return out, rows.Err()
}

// coerce turns what SQLite hands back (it types values, not columns) into
// what the PostgreSQL column takes.
func coerce(v any, pgType string) (any, error) {
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	switch pgType {
	case "bigint", "integer":
		switch x := v.(type) {
		case int64:
			return x, nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			if x == math.Trunc(x) {
				return int64(x), nil
			}
		case string:
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return n, nil
			}
		}
	case "double precision":
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case string:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return f, nil
			}
		}
	case "text":
		switch x := v.(type) {
		case string:
			return x, nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		}
	}
	return nil, fmt.Errorf("can't store %T %v as %s", v, v, pgType)
}
