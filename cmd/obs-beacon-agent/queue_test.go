package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/agentlink"
)

func files(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestQueueSurvivesARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "queue")
	t0 := time.Now()
	q := openQueue(dir, time.Hour, t0)
	q.push(t0, []byte("a"))
	q.push(t0, []byte("b")) // same time: still a later id
	if n := len(files(t, dir)); n != 0 {
		t.Fatalf("%d files before persist: a report sent at once shouldn't touch the disk", n)
	}
	q.persist()
	q.push(t0.Add(time.Second), []byte("c"))
	q.persist()
	_ = os.WriteFile(filepath.Join(dir, ".report-123"), []byte("half"), 0o600) // a write cut short
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)      // not ours: left alone

	q = openQueue(dir, time.Hour, t0.Add(time.Minute))
	var got []string
	for q.len() > 0 {
		b, err := q.peek()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(b))
		q.pop()
	}
	if fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("after a restart got %v, want [a b c]", got)
	}
	if left := files(t, dir); fmt.Sprint(left) != "[notes.txt]" {
		t.Fatalf("left behind: %v", left)
	}
}

func TestQueueDropsWhatsTooOld(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	q := openQueue(dir, time.Hour, t0)
	q.push(t0.Add(-2*time.Hour), []byte("old"))
	q.push(t0.Add(-time.Minute), []byte("new"))
	q.persist()
	q = openQueue(dir, time.Hour, t0)
	if b, _ := q.peek(); q.len() != 1 || string(b) != "new" {
		t.Fatalf("len %d, first %q: want only the report inside the hour", q.len(), b)
	}
	if n := len(files(t, dir)); n != 1 {
		t.Fatalf("%d files, want 1", n)
	}
}

func TestQueueLimits(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Now()
	q := openQueue(dir, 24*time.Hour, t0)
	for i := range maxQueue + 5 {
		q.push(t0.Add(time.Duration(i)*time.Second), []byte(fmt.Sprint(i)))
	}
	q.persist()
	if b, _ := q.peek(); q.len() != maxQueue || string(b) != "5" {
		t.Fatalf("len %d, oldest %q: want %d, oldest 5", q.len(), b, maxQueue)
	}
	if n := len(files(t, dir)); n != maxQueue {
		t.Fatalf("%d files, want %d", n, maxQueue)
	}
	// Past the byte limit the oldest go, but never the newest.
	big := make([]byte, maxQueueBytes/2+1)
	q.push(t0.Add(time.Hour), big)
	q.push(t0.Add(time.Hour+time.Second), big)
	if q.len() != 1 || q.bytes != int64(len(big)) {
		t.Fatalf("len %d, %d bytes: want only the newest", q.len(), q.bytes)
	}
}

func TestQueueWithoutADisk(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o600)
	q := openQueue(filepath.Join(f, "queue"), time.Hour, time.Now()) // under a file: can't be made
	q.push(time.Now(), []byte("a"))
	q.persist()
	if b, err := q.peek(); err != nil || string(b) != "a" {
		t.Fatalf("memory-only queue lost its report: %q %v", b, err)
	}
}

// The agent keeps reports through obs-beacond being away and an agent
// restart, then sends them oldest first, all but the newest as queued.
func TestFlushAfterARestart(t *testing.T) {
	var mu sync.Mutex
	down := true
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			http.Error(w, "upgrading", http.StatusServiceUnavailable)
			return
		}
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		got = append(got, string(b[:n])+"/"+r.Header.Get("X-Beacon-Queued"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("secret\n"), 0o600)
	newAgent := func() *agent {
		link, err := agentlink.New(agentlink.Config{Server: srv.URL, TokenFile: tok, StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return &agent{cfg: &config{Server: srv.URL}, link: link, seen: map[string]bool{},
			queue: openQueue(filepath.Join(dir, "queue"), time.Hour, time.Now())}
	}
	ctx := t.Context()
	a := newAgent()
	for _, r := range []string{"r1", "r2"} {
		a.queue.push(time.Now(), []byte(r))
		a.flush(ctx)
		a.queue.persist()
	}
	if !a.down || a.queue.len() != 2 {
		t.Fatalf("down %v, %d queued: want 2 kept while obs-beacond is away", a.down, a.queue.len())
	}

	a = newAgent() // the agent restarts
	mu.Lock()
	down = false
	mu.Unlock()
	a.queue.push(time.Now(), []byte("r3"))
	a.flush(ctx)
	a.queue.persist()
	if fmt.Sprint(got) != "[r1/1 r2/1 r3/]" || a.queue.len() != 0 {
		t.Fatalf("sent %v, %d left: want [r1/1 r2/1 r3/] and none", got, a.queue.len())
	}
	if n := len(files(t, filepath.Join(dir, "queue"))); n != 0 {
		t.Fatalf("%d files left after sending", n)
	}
}

// After an outage agents come back spread over a few seconds, not at once.
func TestRetryJitter(t *testing.T) {
	if retryJitter(15*time.Second) != 7500*time.Millisecond || retryJitter(time.Minute) != 10*time.Second {
		t.Fatal("jitter bounds")
	}
	a := &agent{jitter: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	a.wait(ctx) // a stopping agent doesn't sit out the wait
	if time.Since(start) > time.Second {
		t.Fatal("wait ignored the context")
	}
	a.jitter = 20 * time.Millisecond
	start = time.Now()
	a.wait(context.Background())
	if time.Since(start) > time.Second {
		t.Fatal("waited past the jitter")
	}
}
