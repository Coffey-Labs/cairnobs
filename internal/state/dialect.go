package state

// The SQL here is written once, in the subset SQLite and PostgreSQL share,
// with ? placeholders (docs/scale-and-availability.md, Storage). A dialect
// covers the few differences: placeholder style, what orders rows that tie,
// the schema's DDL (migrate.go), and how a value is passed. Every query goes
// through exec, query, queryRow or a tx, so none of them can forget.

import (
	"database/sql"
	"strconv"
	"strings"
)

type dialect int

const (
	sqlite dialect = iota
	postgres
)

func (k dialect) String() string {
	if k == postgres {
		return "postgres"
	}
	return "sqlite"
}

// rebind turns ? placeholders into $1, $2… for PostgreSQL. The queries here
// never put a ? inside a string literal, so a plain scan is enough.
func (k dialect) rebind(q string) string {
	if k != postgres || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// args passes flags as 0 and 1: the flag columns are integers in both
// dialects, and PostgreSQL won't take a bool for one.
func (k dialect) args(in []any) []any {
	var out []any // a copy, made only when there's a flag to change
	for i, a := range in {
		if v, ok := a.(bool); ok {
			if out == nil {
				out = append([]any(nil), in...)
			}
			out[i] = 0
			if v {
				out[i] = 1
			}
		}
	}
	if out == nil {
		return in
	}
	return out
}

// rowOrder is a column that orders rows of a table in insertion order, for
// tables with no key of their own: SQLite's hidden rowid, a real id column
// in PostgreSQL.
func (k dialect) rowOrder() string {
	if k == postgres {
		return "id"
	}
	return "rowid"
}

func (d *DB) exec(q string, args ...any) (sql.Result, error) {
	return d.db.Exec(d.kind.rebind(q), d.kind.args(args)...)
}

func (d *DB) query(q string, args ...any) (*sql.Rows, error) {
	return d.db.Query(d.kind.rebind(q), d.kind.args(args)...)
}

func (d *DB) queryRow(q string, args ...any) *sql.Row {
	return d.db.QueryRow(d.kind.rebind(q), d.kind.args(args)...)
}

// txn is a transaction whose Exec, Query and QueryRow go through the dialect.
type txn struct {
	*sql.Tx
	kind dialect
}

func (d *DB) begin() (txn, error) {
	t, err := d.db.Begin()
	return txn{t, d.kind}, err
}

func (t txn) Exec(q string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.kind.rebind(q), t.kind.args(args)...)
}

func (t txn) Query(q string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(t.kind.rebind(q), t.kind.args(args)...)
}

func (t txn) QueryRow(q string, args ...any) *sql.Row {
	return t.Tx.QueryRow(t.kind.rebind(q), t.kind.args(args)...)
}
