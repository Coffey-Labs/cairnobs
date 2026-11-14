package server

import (
	"context"
	"net/http"
	"testing"
	"testing/fstest"
	"time"
)

// /readyz turns ready once the copy has caught up, and unready as soon as
// it's told to stop, while /healthz stays up throughout.
func TestReadyOnlyWhileCaughtUpAndNotStopping(t *testing.T) {
	a, _ := twoCopies(t, func(*Config) {})
	h := a.Handler()
	if rec := get(h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before catching up: %d %s", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.follow(ctx)
	eventually(t, "ready", func() bool { return get(h, "/readyz", nil).Code == http.StatusOK })
	if rec := get(a.AgentHandler(), "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("agent listener: %d", rec.Code)
	}
	a.draining.Store(true)
	if rec := get(h, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopping: %d", rec.Code)
	}
	if rec := get(h, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz while stopping: %d", rec.Code)
	}
}

// Stopping: unready at once, the listeners still open for shutdown_delay,
// then closed.
func TestStopDrains(t *testing.T) {
	a, _ := twoCopies(t, func(c *Config) { c.ShutdownDelay = 300 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.ListenAndServe(ctx) }()
	eventually(t, "ready", func() bool { return isClosed(a.caughtUp) })
	cancel()
	eventually(t, "unready", a.draining.Load)
	select {
	case err := <-done:
		t.Fatalf("closed before shutdown_delay: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never stopped")
	}
}

// During a rollout a page from either version gets its scripts from either
// copy; a script nobody has is a 404, not the page.
func TestCopiesServeEachOthersVersions(t *testing.T) {
	pgOnly(t)
	a, b := twoCopies(t, func(*Config) {})
	a.ui = fstest.MapFS{"_app/immutable/entry/start.OLD1.js": {Data: []byte("old();")}}
	b.ui = fstest.MapFS{"_app/immutable/entry/start.NEW2.js": {Data: []byte("new();")}}
	a.index, b.index = []byte("<!doctype html>a"), []byte("<!doctype html>b")
	// a is the old release, b this one.
	if err := a.db.PutAssets("v0.0.9", map[string][]byte{"_app/immutable/entry/start.OLD1.js": []byte("old();")}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	b.shareAssets()
	for _, c := range []struct {
		srv  *Server
		path string
		want string
	}{
		{b, "/_app/immutable/entry/start.OLD1.js", "old();"}, // the old page's script, from the new copy
		{a, "/_app/immutable/entry/start.NEW2.js", "new();"}, // the new page's, from the old copy
		{a, "/_app/immutable/entry/start.OLD1.js", "old();"}, // its own
	} {
		rec := get(c.srv.Handler(), c.path, nil)
		if rec.Code != 200 || rec.Body.String() != c.want || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s: %d %q %q", c.path, rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
			t.Fatalf("content type %q", ct)
		}
	}
	if rec := get(b.Handler(), "/_app/immutable/entry/start.NONE.js", nil); rec.Code != 404 {
		t.Fatalf("missing script: %d", rec.Code)
	}
}
