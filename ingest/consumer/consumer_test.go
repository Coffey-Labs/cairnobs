package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

type fakeReader struct {
	msgs chan kafka.Message

	mu        sync.Mutex
	committed [][]kafka.Message
}

func newFakeReader() *fakeReader {
	return &fakeReader{msgs: make(chan kafka.Message, 16)}
}

func (f *fakeReader) push(m kafka.Message) { f.msgs <- m }

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	select {
	case m := <-f.msgs:
		return m, nil
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	}
}

func (f *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, msgs)
	return nil
}

func (f *fakeReader) Close() error { return nil }

func (f *fakeReader) commitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.committed)
}

type fakeWriter struct {
	mu       sync.Mutex
	batches  [][]Record
	failNext bool
}

func (f *fakeWriter) WriteBatch(_ context.Context, records []Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("simulated clickhouse failure")
	}
	batch := make([]Record, len(records))
	copy(batch, records)
	f.batches = append(f.batches, batch)
	return nil
}

func (f *fakeWriter) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func newTestConsumer(r reader, w chWriter, cfg Config) *Consumer {
	return &Consumer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		reader: r,
		writer: w,
		cfg:    cfg,
	}
}

func mustMarshal(t *testing.T, rec *logsv1.LogRecord) []byte {
	t.Helper()
	b, err := proto.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestConsumerFlushesOnBatchSize(t *testing.T) {
	fr := newFakeReader()
	fw := &fakeWriter{}
	c := newTestConsumer(fr, fw, Config{BatchMaxSize: 2, FlushIntervalMS: 60_000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	fr.push(kafka.Message{Value: mustMarshal(t, &logsv1.LogRecord{Message: "a"})})
	fr.push(kafka.Message{Value: mustMarshal(t, &logsv1.LogRecord{Message: "b"})})

	waitFor(t, time.Second, func() bool { return fw.batchCount() == 1 })

	fw.mu.Lock()
	if len(fw.batches[0]) != 2 {
		t.Fatalf("expected batch of 2 records, got %d", len(fw.batches[0]))
	}
	fw.mu.Unlock()

	waitFor(t, time.Second, func() bool { return fr.commitCount() == 1 })
}

func TestConsumerFlushesOnTimeout(t *testing.T) {
	fr := newFakeReader()
	fw := &fakeWriter{}
	c := newTestConsumer(fr, fw, Config{BatchMaxSize: 1000, FlushIntervalMS: 20})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	fr.push(kafka.Message{Value: mustMarshal(t, &logsv1.LogRecord{Message: "only-one"})})

	waitFor(t, time.Second, func() bool { return fw.batchCount() == 1 })

	fw.mu.Lock()
	if len(fw.batches[0]) != 1 {
		t.Fatalf("expected batch of 1 record, got %d", len(fw.batches[0]))
	}
	fw.mu.Unlock()
}

func TestConsumerDoesNotCommitOnWriteFailure(t *testing.T) {
	fr := newFakeReader()
	fw := &fakeWriter{failNext: true}
	c := newTestConsumer(fr, fw, Config{BatchMaxSize: 1, FlushIntervalMS: 60_000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	fr.push(kafka.Message{Value: mustMarshal(t, &logsv1.LogRecord{Message: "will-fail"})})

	// Give the flush a moment to run and fail.
	time.Sleep(100 * time.Millisecond)

	if got := fr.commitCount(); got != 0 {
		t.Fatalf("expected no commits after a failed clickhouse write, got %d", got)
	}
	// The batch was attempted even though writer returned an error.
	if fw.batchCount() != 0 {
		t.Fatalf("fakeWriter should not record a failed batch, got %d recorded", fw.batchCount())
	}
}

// TestConsumerExtractsTenantIDFromHeader is the read-side half of the
// producer/consumer tenant_id contract -- ingest/internal/grpcserver
// attaches this header on the way in; this proves the consumer reads it
// back correctly (and that a message with no header at all -- the
// single-tenant/no-resolver case -- gets an empty TenantID, not an
// error).
func TestConsumerExtractsTenantIDFromHeader(t *testing.T) {
	fr := newFakeReader()
	fw := &fakeWriter{}
	c := newTestConsumer(fr, fw, Config{BatchMaxSize: 2, FlushIntervalMS: 60_000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	fr.push(kafka.Message{
		Value:   mustMarshal(t, &logsv1.LogRecord{Message: "tagged"}),
		Headers: []kafka.Header{{Key: TenantIDHeaderKey, Value: []byte("acme")}},
	})
	fr.push(kafka.Message{Value: mustMarshal(t, &logsv1.LogRecord{Message: "untagged"})})

	waitFor(t, time.Second, func() bool { return fw.batchCount() == 1 })

	fw.mu.Lock()
	defer fw.mu.Unlock()
	byMessage := map[string]string{}
	for _, r := range fw.batches[0] {
		byMessage[r.Record.GetMessage()] = r.TenantID
	}
	if byMessage["tagged"] != "acme" {
		t.Fatalf("TenantID for the tagged message = %q, want acme", byMessage["tagged"])
	}
	if byMessage["untagged"] != "" {
		t.Fatalf("TenantID for the untagged message = %q, want empty", byMessage["untagged"])
	}
}
