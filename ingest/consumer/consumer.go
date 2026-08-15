// Package consumer reads normalized-on-write LogRecords back off Redpanda
// and batch-writes them into ClickHouse. Offsets are committed only after
// a successful ClickHouse write, so a ClickHouse outage causes redelivery
// on restart rather than silent data loss (at-least-once, not exactly-once
// — Phase 0 doesn't dedupe on the consumer side).
//
// Moved out of internal/ (was ingest/internal/consumer) once
// enterprise/cmd/enterprise-ingest needed to run this same flush loop
// against a per-tenant chWriter -- see clickhousewriter's doc comment
// for why (same Go internal/-visibility reasoning as every other
// package this phase moved out of internal/ for a cross-module
// import). Each record's TenantID (Record.TenantID below) is read from
// the tenant_id Kafka message header grpcserver.TenantIDHeaderKey
// documents -- empty when no TenantResolver was configured for the
// PushBatch call that produced it, exactly as before per-tenant ingest
// credentials existed. What a chWriter implementation *does* with that
// tag varies: ingest/cmd/ingest's single-tenant clickhousewriter.Writer
// ignores it (writes everything to its one configured database, per
// Phase 0-3 behavior, unchanged); enterprise/internal/chwriter.Registry
// (only ever wired into enterprise/cmd/enterprise-ingest, never this
// core binary) routes each record to its tenant's dedicated ClickHouse
// database instead.
package consumer

import (
	"context"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// TenantIDHeaderKey mirrors ingest/internal/grpcserver.TenantIDHeaderKey
// -- kept as its own constant (not an import of grpcserver, which is
// the agent-facing *producer* side, a different concern from this
// package's consumer side) so this package's dependency list stays
// narrow. Both must name the same literal; a mismatch would silently
// stop tenant_id from ever reaching a consumer, so grpcserver's own
// doc comment on TenantIDHeaderKey cross-references this one.
const TenantIDHeaderKey = "tenant_id"

// Record pairs a parsed LogRecord with the tenant it was tagged with at
// ingest time (see the package doc comment).
type Record struct {
	TenantID string
	Record   *logsv1.LogRecord
}

// chWriter is the subset of a ClickHouse writer this package depends
// on, kept as an interface so the flush loop is unit-testable without a
// real ClickHouse connection, and so both the single-tenant
// (clickhousewriter.Writer, adapted) and multi-tenant
// (enterprise/internal/chwriter.Registry) implementations can share
// this exact same consumer loop.
type chWriter interface {
	WriteBatch(ctx context.Context, records []Record) error
}

// reader is the subset of *kafka.Reader used here, as an interface so the
// flush/commit logic can be tested against a fake without a real broker.
type reader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Config is deliberately a local type, not ingest/internal/config's
// RedpandaConfig/BatchConfig -- same "this package must be importable
// from enterprise/, so it can't depend on ingest/internal/..." reasoning
// as clickhousewriter.Config.
type Config struct {
	Brokers         []string
	Topic           string
	ConsumerGroup   string
	BatchMaxSize    int
	FlushIntervalMS int
}

type Consumer struct {
	logger *slog.Logger
	reader reader
	writer chWriter
	cfg    Config
}

func New(logger *slog.Logger, cfg Config, w chWriter) *Consumer {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: cfg.Brokers,
		Topic:   cfg.Topic,
		GroupID: cfg.ConsumerGroup,
	})
	return &Consumer{logger: logger, reader: r, writer: w, cfg: cfg}
}

func (c *Consumer) Run(ctx context.Context) error {
	defer c.reader.Close()

	flushInterval := time.Duration(c.cfg.FlushIntervalMS) * time.Millisecond
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

	var records []Record
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
			records = append(records, Record{TenantID: tenantIDFromHeaders(m.Headers), Record: &rec})
			pending = append(pending, m)
			if len(records) >= c.cfg.BatchMaxSize {
				flush()
			}
		}
	}
}

func tenantIDFromHeaders(headers []kafka.Header) string {
	for _, h := range headers {
		if h.Key == TenantIDHeaderKey {
			return string(h.Value)
		}
	}
	return ""
}
