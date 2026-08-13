// Package consumer reads normalized-on-write LogRecords back off Redpanda
// and batch-writes them into ClickHouse. Offsets are committed only after
// a successful ClickHouse write, so a ClickHouse outage causes redelivery
// on restart rather than silent data loss (at-least-once, not exactly-once
// — Phase 0 doesn't dedupe on the consumer side).
package consumer

import (
	"context"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	"github.com/sentry/sentry/ingest/internal/config"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// chWriter is the subset of *clickhousewriter.Writer this package depends
// on, kept as an interface so the flush loop is unit-testable without a
// real ClickHouse connection.
type chWriter interface {
	WriteBatch(ctx context.Context, records []*logsv1.LogRecord) error
}

// reader is the subset of *kafka.Reader used here, as an interface so the
// flush/commit logic can be tested against a fake without a real broker.
type reader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

type Consumer struct {
	logger   *slog.Logger
	reader   reader
	writer   chWriter
	batchCfg config.BatchConfig
}

func New(logger *slog.Logger, redpandaCfg config.RedpandaConfig, batchCfg config.BatchConfig, w chWriter) *Consumer {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: redpandaCfg.Brokers,
		Topic:   redpandaCfg.Topic,
		GroupID: redpandaCfg.ConsumerGroup,
	})
	return &Consumer{logger: logger, reader: r, writer: w, batchCfg: batchCfg}
}

func (c *Consumer) Run(ctx context.Context) error {
	defer c.reader.Close()

	flushInterval := time.Duration(c.batchCfg.FlushIntervalMS) * time.Millisecond
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	msgCh := make(chan kafka.Message)
	fetchErrCh := make(chan error, 1)

	go func() {
		for {
			m, err := c.reader.FetchMessage(ctx)
			if err != nil {
				fetchErrCh <- err
				return
			}
			select {
			case msgCh <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	var records []*logsv1.LogRecord
	var pending []kafka.Message

	flush := func() {
		if len(records) == 0 {
			return
		}
		if err := c.writer.WriteBatch(ctx, records); err != nil {
			c.logger.Error("clickhouse batch write failed, offsets not committed, will redeliver",
				"records", len(records), "error", err)
		} else if err := c.reader.CommitMessages(ctx, pending...); err != nil {
			c.logger.Error("committing offsets after clickhouse write", "error", err)
		} else {
			c.logger.Debug("batch flushed to clickhouse", "records", len(records))
		}
		records = records[:0]
		pending = pending[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case err := <-fetchErrCh:
			flush()
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-ticker.C:
			flush()
		case m := <-msgCh:
			var rec logsv1.LogRecord
			if err := proto.Unmarshal(m.Value, &rec); err != nil {
				c.logger.Warn("skipping unparseable message", "error", err, "offset", m.Offset)
				if cerr := c.reader.CommitMessages(ctx, m); cerr != nil {
					c.logger.Error("committing offset for poison message", "error", cerr)
				}
				continue
			}
			records = append(records, &rec)
			pending = append(pending, m)
			if len(records) >= c.batchCfg.MaxSize {
				flush()
			}
		}
	}
}
