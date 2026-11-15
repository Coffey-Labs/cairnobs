package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func actionServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	tok := NewAgentToken()
	srv, err := New(&Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []AgentConfig{{Name: "node1", TokenSHA256: HashToken(tok)}, {Name: "node2", TokenSHA256: HashToken(NewAgentToken())}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, tok
}

// The channel (docs/cert-rotation.md, step 1): a report's answer says an
// action waits, the agent takes it once, and its next report's result is
// recorded and audited.
func TestActionsChannel(t *testing.T) {
	srv, tok := actionServer(t)
	h := srv.Handler()
	rep := func(results ...report.ActionResult) *httptest.ResponseRecorder {
		b, _ := json.Marshal(report.Report{V: report.Version, Actions: results})
		return post(h, "/api/v1/agent/report", tok, b, nil)
	}
	if r := rep(); r.Code != 204 || r.Header().Get("X-Beacon-Actions") != "" {
		t.Fatalf("nothing waiting: %d %q", r.Code, r.Header().Get("X-Beacon-Actions"))
	}
	id, err := srv.queueAction("node1", "reload", map[string]string{"unit": "nginx.service"}, "john", "reload nginx.service on node1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.queueAction("node2", "reload", map[string]string{}, "john", "x"); err != nil {
		t.Fatal(err)
	}
	if r := rep(); r.Header().Get("X-Beacon-Actions") != "1" {
		t.Fatal("not told an action waits")
	}
	take := func() []report.Action {
		req := httptest.NewRequest("GET", "/api/v1/agent/actions", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out struct{ Actions []report.Action }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != 200 || err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return out.Actions
	}
	got := take()
	if len(got) != 1 || got[0].ID != id || got[0].Kind != "reload" || string(got[0].Args) != `{"unit":"nginx.service"}` {
		t.Fatalf("%+v", got)
	}
	if again := take(); len(again) != 0 {
		t.Fatalf("handed over twice: %+v", again)
	}
	if r := rep(report.ActionResult{ID: id, OK: true, Detail: "reloaded nginx.service"}); r.Header().Get("X-Beacon-Actions") != "" {
		t.Fatal("still says one waits")
	}

	c := devCookie(t, h)
	var list struct {
		Actions   []ActionView
		Available bool
	}
	_ = json.Unmarshal(get(h, "/api/v1/actions", c).Body.Bytes(), &list)
	if !list.Available || len(list.Actions) != 2 {
		t.Fatalf("%+v", list)
	}
	for _, a := range list.Actions {
		switch a.Agent {
		case "node1":
			if a.State != "done" || a.Detail != "reloaded nginx.service" || a.By != "john" {
				t.Errorf("node1: %+v", a)
			}
		case "node2":
			if a.State != "waiting" {
				t.Errorf("node2: %+v", a)
			}
		}
	}
	audit := get(h, "/api/v1/audit", c).Body.String()
	if !strings.Contains(audit, "asked node1 to act") || !strings.Contains(audit, "did action "+id) {
		t.Errorf("audit: %s", audit)
	}
}
