// Package producer wraps the Redpanda (Kafka API) producer used by the
// gRPC front end to forward agent-submitted batches onto the transport
// layer, unchanged. OTel-log-shape normalization happens later, on the
// consumer side — see internal/normalize.
package producer

import (
	"context"

	"github.com/segmentio/kafka-go"

	"github.com/cairnobs/cairnobs/ingest/internal/config"
)

type Producer struct {
	writer *kafka.Writer
}

func New(cfg config.RedpandaConfig) *Producer {
	return &Producer{
		writer: &kafka.Writer{
			Addr:  kafka.TCP(cfg.Brokers...),
			Topic: cfg.Topic,
			// Partition by host so a single host's records stay in
			// relative order within a partition.
			Balancer:               &kafka.Hash{},
			RequiredAcks:           kafka.RequireOne,
			AllowAutoTopicCreation: false, // topics are provisioned explicitly, see /transport
		},
	}
}

func (p *Producer) Close() error {
	return p.writer.Close()
}

// WriteBatch writes all messages in one call. kafka-go's WriteMessages
// either succeeds for the whole batch or returns an error, which matches
// the PushBatch RPC's all-or-nothing contract for Phase 0.
func (p *Producer) WriteBatch(ctx context.Context, msgs []kafka.Message) error {
	return p.writer.WriteMessages(ctx, msgs...)
}
