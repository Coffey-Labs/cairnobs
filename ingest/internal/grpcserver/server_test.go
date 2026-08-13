package grpcserver

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	"github.com/sentry/sentry/ingest/internal/config"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

type fakeProducer struct {
	mu      sync.Mutex
	written [][]kafka.Message
	err     error
}

func (f *fakeProducer) WriteBatch(_ context.Context, msgs []kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	batch := make([]kafka.Message, len(msgs))
	copy(batch, msgs)
	f.written = append(f.written, batch)
	return nil
}

func newTestServer(p batchProducer) *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), config.GRPCConfig{}, config.TLSConfig{}, p)
}

func TestPushBatchAssignsRecordID(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	req := &logsv1.PushBatchRequest{
		BatchId: "b1",
		Records: []*logsv1.LogRecord{
			{Host: "h1", Message: "one"},
			{Host: "h1", Message: "two"},
		},
	}

	resp, err := s.PushBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}
	if resp.GetAccepted() != 2 {
		t.Fatalf("Accepted = %d, want 2", resp.GetAccepted())
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 1 || len(fp.written[0]) != 2 {
		t.Fatalf("unexpected written batches: %+v", fp.written)
	}

	seen := make(map[string]bool)
	for _, m := range fp.written[0] {
		var rec logsv1.LogRecord
		if err := proto.Unmarshal(m.Value, &rec); err != nil {
			t.Fatalf("unmarshaling produced message: %v", err)
		}
		if rec.GetRecordId() == "" {
			t.Fatalf("record_id was not assigned for message %q", rec.GetMessage())
		}
		if seen[rec.GetRecordId()] {
			t.Fatalf("duplicate record_id %q across records in the same batch", rec.GetRecordId())
		}
		seen[rec.GetRecordId()] = true
	}
}

func TestPushBatchOverwritesAgentSuppliedRecordID(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	req := &logsv1.PushBatchRequest{
		Records: []*logsv1.LogRecord{
			{Host: "h1", Message: "one", RecordId: "agent-supplied-should-be-ignored"},
		},
	}

	if _, err := s.PushBatch(context.Background(), req); err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	var rec logsv1.LogRecord
	if err := proto.Unmarshal(fp.written[0][0].Value, &rec); err != nil {
		t.Fatalf("unmarshaling produced message: %v", err)
	}
	if rec.GetRecordId() == "agent-supplied-should-be-ignored" {
		t.Fatal("expected ingest to overwrite any agent-supplied record_id")
	}
	if rec.GetRecordId() == "" {
		t.Fatal("expected a server-assigned record_id")
	}
}

func TestPushBatchEmptyRecordsIsANoOp(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	resp, err := s.PushBatch(context.Background(), &logsv1.PushBatchRequest{})
	if err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}
	if resp.GetAccepted() != 0 {
		t.Fatalf("Accepted = %d, want 0", resp.GetAccepted())
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 0 {
		t.Fatalf("expected no batches written for an empty request, got %d", len(fp.written))
	}
}
