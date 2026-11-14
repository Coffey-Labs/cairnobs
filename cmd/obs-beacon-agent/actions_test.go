package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cairnobs/cairnobs/internal/agentlink"
)

// The agent fetches its actions when told, runs the ones it knows, and
// says why not for the rest; results go out with the next report.
func TestTakeActions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/report":
			w.Header().Set("X-Beacon-Actions", "1")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/actions":
			_, _ = w.Write([]byte(`{"actions":[{"id":"a1","kind":"test-ok","args":{"n":1}},{"id":"a2","kind":"test-no","args":{}},{"id":"a3","kind":"teleport","args":{}}]}`))
		}
	}))
	defer srv.Close()
	actionDoers["test-ok"] = func(a *agent, ctx context.Context, args json.RawMessage) (string, error) {
		return "did " + string(args), nil
	}
	actionDoers["test-no"] = func(a *agent, ctx context.Context, args json.RawMessage) (string, error) {
		return "", errors.New("not allowed here")
	}
	defer delete(actionDoers, "test-ok")
	defer delete(actionDoers, "test-no")
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("secret\n"), 0o600)
	link, err := agentlink.New(agentlink.Config{Server: srv.URL, TokenFile: tok, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: &config{Server: srv.URL}, link: link, seen: map[string]bool{}}
	if err := a.send(t.Context(), []byte("{}"), false); err != nil || !a.actionsWant {
		t.Fatalf("%v, want %v", err, a.actionsWant)
	}
	a.actionsWant = false
	a.takeActions(t.Context())
	if len(a.results) != 3 {
		t.Fatalf("%+v", a.results)
	}
	if r := a.results[0]; r.ID != "a1" || !r.OK || r.Detail != `did {"n":1}` {
		t.Errorf("%+v", r)
	}
	if r := a.results[1]; r.OK || r.Detail != "not allowed here" {
		t.Errorf("%+v", r)
	}
	if r := a.results[2]; r.OK || r.Detail != "this agent doesn't do teleport actions (it may be older than obs-beacond)" {
		t.Errorf("%+v", r)
	}
}
