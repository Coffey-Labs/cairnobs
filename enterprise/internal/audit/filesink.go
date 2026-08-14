package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// FileSink is a working CheckpointSink appropriate for development and
// testing -- appends one JSON line per checkpoint to a local file.
// **Not a real external-anchoring guarantee**: a local file on the same
// host as Postgres is reachable by exactly the kind of privileged actor
// checkpointing is meant to defend against. A production deployment
// needs a genuinely separate-trust-domain sink (S3 with Object Lock, a
// separate append-only service) -- deliberately not implemented here,
// per checkpoint.go's doc comment.
type FileSink struct {
	path string
}

func NewFileSink(path string) *FileSink {
	return &FileSink{path: path}
}

type fileSinkLine struct {
	FromID             int64     `json:"from_id"`
	ToID               int64     `json:"to_id"`
	PrevCheckpointHash string    `json:"prev_checkpoint_hash"`
	Hash               string    `json:"hash"`
	CreatedAt          time.Time `json:"created_at"`
}

func (f *FileSink) Write(_ context.Context, cp Checkpoint) error {
	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: opening checkpoint file: %w", err)
	}
	defer file.Close()

	line := fileSinkLine{
		FromID: cp.FromID, ToID: cp.ToID,
		PrevCheckpointHash: cp.PrevCheckpointHash, Hash: cp.Hash, CreatedAt: cp.CreatedAt,
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("audit: encoding checkpoint: %w", err)
	}
	if _, err := fmt.Fprintln(file, string(encoded)); err != nil {
		return fmt.Errorf("audit: writing checkpoint: %w", err)
	}
	return nil
}

func (f *FileSink) LastCheckpoint(_ context.Context) (*Checkpoint, error) {
	file, err := os.Open(f.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: opening checkpoint file: %w", err)
	}
	defer file.Close()

	var lastLine string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("audit: reading checkpoint file: %w", err)
	}
	if lastLine == "" {
		return nil, nil
	}

	var line fileSinkLine
	if err := json.Unmarshal([]byte(lastLine), &line); err != nil {
		return nil, fmt.Errorf("audit: decoding last checkpoint line: %w", err)
	}
	return &Checkpoint{
		FromID: line.FromID, ToID: line.ToID,
		PrevCheckpointHash: line.PrevCheckpointHash, Hash: line.Hash, CreatedAt: line.CreatedAt,
	}, nil
}
