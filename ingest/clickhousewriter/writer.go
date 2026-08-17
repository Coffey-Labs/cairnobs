// Package clickhousewriter batch-inserts normalized log rows into
// ClickHouse using the native protocol driver's batch API. Moved out of
// internal/ (was ingest/internal/clickhousewriter) once
// enterprise/internal/chwriter needed to construct one *Writer per
// tenant -- same reasoning api/internal/dashboards and friends moved out
// of internal/ earlier in Phase 4: Go's compiler-enforced internal/
// visibility blocks a separate module (enterprise/) from importing
// anything under ingest/internal/..., independent of licensing --
// both modules are AGPLv3 as of Phase 6, and this was always an
// import-graph constraint, not a license one.
package clickhousewriter

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/sentry/sentry/ingest/internal/normalize"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// Config is deliberately a local type, not ingest/internal/config.
// ClickHouseConfig -- this package needs to be importable from
// enterprise/ (see the package doc comment), and internal/config must
// stay internal (nothing outside ingest/ needs its other fields, e.g.
// TLSConfig/GRPCConfig, and moving the whole package out just for this
// one struct would be a wider hole than necessary). Same "narrow local
// type, not the storage/config type itself" precedent as
// enterprise/internal/chrunner.DataSource.
type Config struct {
	Addr     string
	Database string
	Username string
	Password string
}

type Writer struct {
	conn driver.Conn
}

func New(ctx context.Context, cfg Config) (*Writer, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("opening clickhouse connection: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging clickhouse: %w", err)
	}
	return &Writer{conn: conn}, nil
}

func (w *Writer) Close() error {
	return w.conn.Close()
}

func (w *Writer) WriteBatch(ctx context.Context, records []*logsv1.LogRecord) error {
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO logs (timestamp, host, service, severity, message, attributes, record_id)")
	if err != nil {
		return fmt.Errorf("preparing batch: %w", err)
	}

	for _, rec := range records {
		row := normalize.ToRow(rec)
		if err := batch.Append(row.Timestamp, row.Host, row.Service, row.Severity, row.Message, row.Attributes, row.RecordID); err != nil {
			return fmt.Errorf("appending row to batch: %w", err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("sending batch: %w", err)
	}
	return nil
}
