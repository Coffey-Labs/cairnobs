package clusterwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
)

// fakeAPI serves pods: lists in pages, and watches that send what's queued
// for them. Every other collection is empty; cronjobs aren't served.
type fakeAPI struct {
	mu      sync.Mutex
	pods    []string // uids
	rv      int
	events  chan string // raw watch lines for the next watch
	lists   int
	gone    bool // the next watch answers 410 in its stream
	pageMax int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sa-token" {
		http.Error(w, `{"kind":"Status","code":401,"message":"Unauthorized"}`, 401)
		return
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/version":
		fmt.Fprint(w, `{"gitVersion":"v1.32.2"}`)
	case r.URL.Path == "/apis/batch/v1/cronjobs":
		w.WriteHeader(404)
		fmt.Fprint(w, `{"kind":"Status","code":404,"reason":"NotFound"}`)
	case r.URL.Path != "/api/v1/pods" && q.Get("watch") == "":
		fmt.Fprint(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
	case r.URL.Path != "/api/v1/pods":
		<-r.Context().Done()
	case q.Get("watch") == "":
		f.mu.Lock()
		f.lists++
		start, _ := strconv.Atoi(q.Get("continue"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		limit = min(limit, f.pageMax)
		end := min(start+limit, len(f.pods))
		var items []json.RawMessage
		for _, id := range f.pods[start:end] {
			items = append(items, json.RawMessage(podJSON(id, "Running")))
		}
		cont := ""
		if end < len(f.pods) {
			cont = strconv.Itoa(end)
		}
		rv := f.rv
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": strconv.Itoa(rv), "continue": cont}, "items": items})
		w.Write(b)
	default:
		f.mu.Lock()
		gone := f.gone
		f.gone = false
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if gone {
			fmt.Fprint(w, `{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired"}}`+"\n")
			return
		}
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-f.events:
				if !ok {
					return
				}
				fmt.Fprintln(w, ev)
				w.(http.Flusher).Flush()
			}
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func TestWatchListsWatchesAndListsAgain(t *testing.T) {
	f := &fakeAPI{pods: []string{"a", "b", "c", "d", "e"}, rv: 10, events: make(chan string, 10), pageMax: 2}
	ts := httptest.NewTLSServer(f)
	defer ts.Close()
	tok := t.TempDir() + "/token"
	_ = writeFile(tok, "sa-token\n")
	kc := kube.New(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig, tok)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore()
	for _, r := range resources {
		go watchResource(ctx, kc, s, r)
	}
	go watchVersion(ctx, kc, s)
	eventually(t, "every resource listed", s.allSynced)
	count := func() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.objs["pods"]) }
	if n := count(); n != 5 {
		t.Fatalf("listed %d pods in pages of 2, want 5", n)
	}
	// Changes through the watch; a bookmark doesn't reach the store.
	f.events <- `{"type":"ADDED","object":` + podJSON("f", "Pending") + `}`
	f.events <- `{"type":"DELETED","object":` + podJSON("a", "Running") + `}`
	f.events <- `{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"15"}}}`
	eventually(t, "the watch's changes", func() bool {
		return count() == 5 && s.snapshot()["pods"]["f"] != nil && s.snapshot()["pods"]["a"] == nil
	})
	// The watch ends; the next one is told its version is gone: list again.
	f.mu.Lock()
	f.gone, f.pods = true, []string{"x"}
	lists := f.lists
	f.mu.Unlock()
	close(f.events)
	eventually(t, "listed again after 410", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.lists > lists && count() == 1 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != "v1.32.2" || len(s.problem) != 0 {
		t.Fatalf("version %q, problems %v (cronjobs not served is no problem)", s.version, s.problem)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
