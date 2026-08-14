package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Checkpoint is a rolling hash over a range of audit_log rows, chained
// to the previous checkpoint the same way row_hash chains individual
// rows. The chain in audit_log alone only proves internal consistency:
// anyone with enough Postgres privilege to wipe the table can
// regenerate a perfectly self-consistent new chain from row 1.
// Checkpoints answer a different question -- "does what's in Postgres
// right now match what it was an hour ago" -- but only if they're
// written somewhere the same privileged actor can't also reach. That's
// CheckpointSink's job, not this package's: this package computes
// checkpoints correctly and hands them to a sink; it does not claim any
// particular sink is actually tamper-proof.
type Checkpoint struct {
	FromID             int64
	ToID               int64
	PrevCheckpointHash string
	Hash               string
	CreatedAt          time.Time
}

// CheckpointSink persists checkpoints somewhere external. FileSink
// (below) is a working, testable implementation appropriate for
// development -- a real deployment needs a sink that genuinely isn't
// reachable by whatever could tamper with Postgres (S3 with Object
// Lock, a separate append-only service, etc.), which this package
// deliberately does not implement: that's an operational/infrastructure
// decision, not something to hardcode a specific cloud vendor's SDK for
// without discussing the dependency first.
type CheckpointSink interface {
	// LastCheckpoint returns the most recently written checkpoint, or
	// nil if none exists yet.
	LastCheckpoint(ctx context.Context) (*Checkpoint, error)
	Write(ctx context.Context, cp Checkpoint) error
}

// Checkpointer periodically rolls up new audit_log rows since the last
// checkpoint into a new one.
type Checkpointer struct {
	store *Store
	sink  CheckpointSink
}

func NewCheckpointer(store *Store, sink CheckpointSink) *Checkpointer {
	return &Checkpointer{store: store, sink: sink}
}

// Run computes and writes at most one new checkpoint covering every
// audit_log row added since the last one. Returns (nil, nil) if there's
// nothing new to checkpoint. Call on a schedule (e.g. hourly) from
// cmd/enterprise-auth -- this package doesn't run its own ticker, same
// "caller owns scheduling" shape as /alerting's evaluator.
func (c *Checkpointer) Run(ctx context.Context) (*Checkpoint, error) {
	last, err := c.sink.LastCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit: reading last checkpoint: %w", err)
	}

	fromID := int64(1)
	prevHash := ""
	if last != nil {
		fromID = last.ToID + 1
		prevHash = last.Hash
	}

	rowHashes, maxID, err := c.store.rowHashesFrom(ctx, fromID)
	if err != nil {
		return nil, fmt.Errorf("audit: reading rows for checkpoint: %w", err)
	}
	if len(rowHashes) == 0 {
		return nil, nil
	}

	h := sha256.New()
	h.Write([]byte(prevHash))
	for _, rh := range rowHashes {
		h.Write([]byte{0})
		h.Write([]byte(rh))
	}

	cp := Checkpoint{
		FromID: fromID, ToID: maxID,
		PrevCheckpointHash: prevHash,
		Hash:               hex.EncodeToString(h.Sum(nil)),
		CreatedAt:          time.Now().UTC(),
	}
	if err := c.sink.Write(ctx, cp); err != nil {
		return nil, fmt.Errorf("audit: writing checkpoint: %w", err)
	}
	return &cp, nil
}

// rowHashesFrom returns row_hash values for id >= fromID, in id order,
// plus the highest id seen (so the caller knows where the next
// checkpoint should resume).
func (s *Store) rowHashesFrom(ctx context.Context, fromID int64) ([]string, int64, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, row_hash FROM audit_log WHERE id >= $1 ORDER BY id ASC", fromID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var hashes []string
	var maxID int64
	for rows.Next() {
		var id int64
		var hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, 0, err
		}
		hashes = append(hashes, hash)
		maxID = id
	}
	return hashes, maxID, rows.Err()
}
