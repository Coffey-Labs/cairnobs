package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// A snapshot into a folder that isn't there yet, as on a fresh volume,
// makes the folder.
func TestSnapshotMakesItsFolder(t *testing.T) {
	statetest.SQLiteOnly(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	_ = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k)), 0o600)
	snap := filepath.Join(dir, "backup", "nightly", "beacon.db")
	srv, err := New(&Config{Listen: "127.0.0.1:0", StaleAfter: time.Minute, Database: filepath.Join(dir, "beacon.db"),
		StateKeyFile: key, Snapshot: snap, SnapshotEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // write once, then stop
	srv.snapshots(ctx)
	if st, err := os.Stat(snap); err != nil || st.Size() == 0 {
		t.Fatalf("no snapshot: %v", err)
	}
}
