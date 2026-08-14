package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFileSinkRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.jsonl")
	sink := NewFileSink(path)
	ctx := context.Background()

	none, err := sink.LastCheckpoint(ctx)
	if err != nil {
		t.Fatalf("LastCheckpoint on a nonexistent file: %v", err)
	}
	if none != nil {
		t.Fatalf("expected nil for a nonexistent checkpoint file, got %+v", none)
	}

	cp1 := Checkpoint{FromID: 1, ToID: 10, Hash: "hash1", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := sink.Write(ctx, cp1); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cp2 := Checkpoint{FromID: 11, ToID: 20, PrevCheckpointHash: "hash1", Hash: "hash2", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := sink.Write(ctx, cp2); err != nil {
		t.Fatalf("Write: %v", err)
	}

	last, err := sink.LastCheckpoint(ctx)
	if err != nil {
		t.Fatalf("LastCheckpoint: %v", err)
	}
	if last == nil {
		t.Fatalf("expected a checkpoint, got nil")
	}
	if last.ToID != cp2.ToID || last.Hash != cp2.Hash {
		t.Fatalf("got %+v, want the most recently written checkpoint %+v", last, cp2)
	}
}
