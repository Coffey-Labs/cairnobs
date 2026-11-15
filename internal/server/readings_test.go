package server

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/mailmon"
)

// The evaluator's readings reach the other copy whole: Cloudflare's DNS
// records (which its page leaves out), Patroni's members and errors, and
// the mail round trip's history.
func TestReadingsReachTheOtherCopy(t *testing.T) {
	a, b := twoCopies(t, func(c *Config) {
		c.DataLayer = &DataLayerConfig{Patroni: []string{"http://10.0.0.1:8008", "http://10.0.0.2:8008"}}
	})
	dir := t.TempDir()
	tf, pf := filepath.Join(dir, "token"), filepath.Join(dir, "pw")
	_ = os.WriteFile(tf, []byte("x"), 0o600)
	_ = os.WriteFile(pf, []byte("x"), 0o600)
	for _, s := range []*Server{a, b} {
		s.cf, _ = cloudflare.New(cloudflare.Config{TokenFile: tf})
		s.mail, _ = mailmon.New(mailmon.Config{Account: "monitor@example.test", PasswordFile: pf, Server: "mail.example.test",
			ClientID: "cairn-obs", RedirectURL: "https://obs.example.test/cb", Host: "node1"})
	}
	now := time.Now().Truncate(time.Second)
	a.cf.Set(cloudflare.Snapshot{Checked: now, ErrorSince: now.Add(-time.Hour), Zones: []cloudflare.Zone{
		{Name: "example.com", SSL: "strict", DNS: 1, Records: []cloudflare.Record{{Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: 1}}}}})
	a.patroni.reads = map[string]patroniRead{
		"http://10.0.0.1:8008": {at: now, members: []patroniMember{{Name: "node1", Role: "leader", State: "running"}}, node: &patroniNode{DCSLastSeen: 5}},
		"http://10.0.0.2:8008": {at: now, err: context.DeadlineExceeded},
	}
	a.mail.Set(mailmon.Snapshot{Checked: now, History: []mailmon.TripPoint{{At: now, Seconds: 2.5}}})
	for _, r := range []string{readingCloudflare, readingPatroni, readingMail} {
		a.saveReading(r, now)
		if at, same := b.loadReading(r); !at.Equal(now) || !same {
			t.Fatalf("%s: at %v, same settings %v", r, at, same)
		}
	}
	z := b.cf.Snapshot().Zones
	if len(z) != 1 || len(z[0].Records) != 1 || z[0].Records[0].Content != "192.0.2.1" || !b.cf.Snapshot().ErrorSince.Equal(now.Add(-time.Hour)) {
		t.Fatalf("cloudflare: %+v", b.cf.Snapshot())
	}
	r1, r2 := b.patroni.reads["http://10.0.0.1:8008"], b.patroni.reads["http://10.0.0.2:8008"]
	if r1.members[0].Role != "leader" || r1.node == nil || r1.err != nil || r2.err == nil || r2.err.Error() != "context deadline exceeded" {
		t.Fatalf("patroni: %+v %+v", r1, r2)
	}
	if h := b.mail.Snapshot().History; len(h) != 1 || h[0].Seconds != 2.5 {
		t.Fatalf("mail: %+v", h)
	}
	// Taken already: not read again.
	b.cf.Set(cloudflare.Snapshot{})
	b.loadReading(readingCloudflare)
	if len(b.cf.Snapshot().Zones) != 0 {
		t.Fatal("read the same reading twice")
	}
}

// A copy that becomes the evaluator, or restarts, polls when the stored
// reading is due, not at once; unless the settings changed. A round cut
// short by losing the lease leaves the reading as it was.
func TestPollingStartsWhenDue(t *testing.T) {
	a, b := twoCopies(t, func(c *Config) { c.DataLayer = &DataLayerConfig{Patroni: []string{"http://10.0.0.1:8008"}} })
	var rounds atomic.Int32
	round := func(ctx context.Context) {
		rounds.Add(1)
		a.patroni.mu.Lock()
		a.patroni.reads["http://10.0.0.1:8008"] = patroniRead{at: time.Now(), members: []patroniMember{{Name: "node1"}}}
		a.patroni.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go a.poll(ctx, readingPatroni, time.Hour, round)
	waitFor(t, func() bool { _, at, err := a.db.Reading(readingPatroni); return err == nil && !at.IsZero() })
	cancel()
	if rounds.Load() != 1 {
		t.Fatalf("%d rounds", rounds.Load())
	}

	// b takes over: the reading is fresh, so b waits for it to be due.
	var bRounds atomic.Int32
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.poll(ctx, readingPatroni, time.Hour, func(context.Context) { bRounds.Add(1) }); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if bRounds.Load() != 0 || len(b.patroni.reads) != 1 {
		t.Fatalf("b polled at once (%d rounds) or didn't start from the reading (%+v)", bRounds.Load(), b.patroni.reads)
	}

	// Other settings: polled at once. Cut short, the round is undone.
	b.cfg.DataLayer.Patroni = []string{"http://10.0.0.9:8008"}
	ctx, cancel = context.WithCancel(context.Background())
	started := make(chan struct{})
	done = make(chan struct{})
	go func() {
		b.poll(ctx, readingPatroni, time.Hour, func(ctx context.Context) {
			b.patroni.mu.Lock()
			b.patroni.reads["http://10.0.0.9:8008"] = patroniRead{at: time.Now(), err: context.Canceled}
			b.patroni.mu.Unlock()
			close(started)
			<-ctx.Done()
		})
		close(done)
	}()
	<-started
	cancel()
	<-done
	if _, ok := b.patroni.reads["http://10.0.0.9:8008"]; ok || len(b.patroni.reads) != 1 {
		t.Fatalf("a round cut short was kept: %+v", b.patroni.reads)
	}
}
