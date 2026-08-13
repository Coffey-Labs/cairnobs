// Package clickhousewriter batch-inserts normalized log rows into
// ClickHouse using the native protocol driver's batch API.
package clickhousewriter

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/sentry/sentry/ingest/internal/config"
	"github.com/sentry/sentry/ingest/internal/normalize"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

type Writer struct {
	conn driver.Conn
}

func New(ctx context.Context, cfg config.ClickHouseConfig) (*Writer, error) {
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
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO logs (timestamp, host, service, severity, message, attributes)")
	if err != nil {
		return fmt.Errorf("preparing batch: %w", err)
	}

	for _, rec := range records {
		row := normalize.ToRow(rec)
		if err := batch.Append(row.Timestamp, row.Host, row.Service, row.Severity, row.Message, row.Attributes); err != nil {
			return fmt.Errorf("appending row to batch: %w", err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("sending batch: %w", err)
	}
	return nil
}
