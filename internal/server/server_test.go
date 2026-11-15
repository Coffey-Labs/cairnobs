package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func testServer(t *testing.T, dev bool) (*Server, string) {
	t.Helper()
	tok := NewAgentToken()
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: dev, StaleAfter: time.Minute,
		Agents: []AgentConfig{{Name: "host1", TokenSHA256: HashToken(tok)}, {Name: "host2", TokenSHA256: HashToken(NewAgentToken())}}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv, tok
}

func post(h http.Handler, path, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReportNeedsAKnownToken(t *testing.T) {
	srv, tok := testServer(t, false)
	h := srv.Handler()
	body, _ := json.Marshal(report.Report{V: report.Version, Host: "pretends-to-be-host2", CPU: 12})

	if rec := post(h, "/api/v1/agent/report", "", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", rec.Code)
	}
	if rec := post(h, "/api/v1/agent/report", "wrong", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d", rec.Code)
	}
	if rec := post(h, "/api/v1/agent/report", tok, body, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("good token: got %d %s", rec.Code, rec.Body)
	}
	// The token decides which host this is, not the body.
	snap := srv.store.Snapshot()
	if snap[0].name != "host1" || snap[0].seen.IsZero() || !snap[1].seen.IsZero() {
		t.Fatalf("report landed on the wrong host: %+v", snap)
	}
	old, _ := json.Marshal(report.Report{V: 99})
	if rec := post(h, "/api/v1/agent/report", tok, old, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong protocol version: got %d", rec.Code)
	}
}

// A report from the agent's queue is history: it adds to the sparklines
// but doesn't become the host's state, which a newer report sets.
func TestQueuedReportIsHistoryOnly(t *testing.T) {
	srv, tok := testServer(t, false)
	h := srv.Handler()
	now, _ := json.Marshal(report.Report{V: report.Version, CPU: 10})
	old, _ := json.Marshal(report.Report{V: report.Version, CPU: 90})
	if rec := post(h, "/api/v1/agent/report", tok, now, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("report: got %d", rec.Code)
	}
	seen := srv.store.Snapshot()[0].seen
	if rec := post(h, "/api/v1/agent/report", tok, old, map[string]string{"X-Beacon-Queued": "1"}); rec.Code != http.StatusNoContent {
		t.Fatalf("queued report: got %d", rec.Code)
	}
	got := srv.store.Snapshot()[0]
	if got.last.CPU != 10 || !got.seen.Equal(seen) {
		t.Fatalf("a queued report replaced the state: cpu %v, seen %v (was %v)", got.last.CPU, got.seen, seen)
	}
	if len(got.cpu) != 2 || got.cpu[1] != 90 {
		t.Fatalf("history = %v, want [10 90]", got.cpu)
	}
}

func TestBrowserAPINeedsASession(t *testing.T) {
	srv, _ := testServer(t, true)
	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/overview", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"dev":true`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	// Dev sign-in refuses a request without X-Beacon (a cross-site form can't add it).
	if rec := post(h, "/auth/dev", "", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("dev sign-in without header: got %d", rec.Code)
	}
	rec = post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("dev sign-in: got %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie not locked down: %+v", cookie)
	}
	req := httptest.NewRequest("GET", "/api/v1/overview", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"waiting"`) {
		t.Fatalf("overview: got %d %s", rec.Code, rec.Body)
	}
	// Lists are [] when empty, never null: the UI reads .length on them.
	if strings.Contains(rec.Body.String(), "null") && !strings.Contains(rec.Body.String(), `"last_seen":null`) {
		t.Fatalf("overview has a null list: %s", rec.Body)
	}
	for _, want := range []string{`"attention":[]`, `"cpu":[]`, `"disks":[]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("overview lacks %s: %s", want, rec.Body)
		}
	}
}

func TestDevSigninOffWhenNotConfigured(t *testing.T) {
	srv, _ := testServer(t, false)
	if rec := post(srv.Handler(), "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDevSigninOnlyOnLoopback(t *testing.T) {
	dir := t.TempDir()
	write := func(listen string) string {
		p := filepath.Join(dir, "c.yaml")
		_ = os.WriteFile(p, []byte("listen: \""+listen+"\"\ndev_signin: true\n"), 0o600)
		return p
	}
	for _, ok := range []string{"127.0.0.1:8150", "[::1]:8150", "localhost:8150"} {
		if _, err := LoadConfig(write(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8150", "10.0.0.3:8443", ":8150"} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Errorf("%s: dev sign-in accepted off loopback", bad)
		}
	}
}

func TestJudge(t *testing.T) {
	e := &Expected{
		Hosts: map[string]ExpectedHost{"host2": {
			Running: []string{"mastodon-*", "inbuxa-demo", "gitea-runner"},
			Stopped: []StoppedRule{{Match: "cairnobs-demo-*", Note: "paused"}},
		}},
		CIJobs: []string{"GITEA-ACTIONS-TASK-*"},
	}
	list := []report.Container{
		{Name: "mastodon-web", State: "running", Health: "healthy"},
		{Name: "mastodon-db", State: "running"},
		{Name: "inbuxa-demo", State: "restarting"},
		{Name: "cairnobs-demo-web", State: "exited"},
		{Name: "surprise", State: "running"},
		{Name: "GITEA-ACTIONS-TASK-12_build", State: "running"},
	}
	rows, sum := e.Judge("host2", list)
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.Verdict.Severity + "/" + r.Verdict.Word
	}
	want := map[string]string{
		"mastodon-web":      "good/Running",
		"mastodon-db":       "good/Running",
		"inbuxa-demo":       "critical/Restarting",
		"cairnobs-demo-web": "off/Stopped on purpose",
		"surprise":          "warning/Not expected",
		"gitea-runner":      "critical/Missing",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if _, listed := got["GITEA-ACTIONS-TASK-12_build"]; listed {
		t.Error("CI job container was listed")
	}
	if sum != (ContainerSummary{Running: 2, Problems: 3, Stopped: 1, CI: 1, Total: 6}) {
		t.Errorf("summary %+v", sum)
	}
	// A host with no expectations is judged on Docker's state alone.
	rows, _ = e.Judge("laptop", []report.Container{{Name: "x", State: "running"}, {Name: "y", State: "exited"}})
	if rows[0].Verdict.Severity != "good" || rows[1].Verdict.Severity != "off" {
		t.Errorf("no expectations: %+v", rows)
	}
}

func TestCSPHashesTheBootstrapScript(t *testing.T) {
	csp := contentSecurityPolicy([]byte(`<html><script type="module" src="/x.js"></script><script>start()</script></html>`))
	if !strings.Contains(csp, "'sha256-") || strings.Contains(csp, "unsafe-inline' 'sha") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("csp: %s", csp)
	}
}

func TestAgentListenerSplitsTheRoutes(t *testing.T) {
	srv, tok := testServer(t, false)
	srv.cfg.AgentListen = "127.0.0.1:0"
	body, _ := json.Marshal(report.Report{V: report.Version})
	// The browser side no longer takes reports...
	if rec := post(srv.Handler(), "/api/v1/agent/report", tok, body, nil); rec.Code == http.StatusNoContent {
		t.Fatal("browser listener accepted a report")
	}
	// ...the agent side does, and serves nothing else.
	if rec := post(srv.AgentHandler(), "/api/v1/agent/report", tok, body, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("agent listener: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	srv.AgentHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/overview", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent listener served the browser API: %d", rec.Code)
	}
}

func TestExpectedReloadsWhenTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "expected.yaml")
	write := func(body string, mod time.Time) {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mod, mod)
	}
	t0 := time.Now().Add(-time.Hour)
	write("hosts:\n  host1:\n    running: [a]\n", t0)
	srv, _ := testServer(t, false)
	srv.cfg.Expected = p
	srv.expStat = nil
	if got := srv.currentExpected().Hosts["host1"].Running; len(got) != 1 {
		t.Fatalf("first load: %v", got)
	}
	write("hosts:\n  host1:\n    running: [a, b]\n", t0.Add(time.Minute))
	if got := srv.currentExpected().Hosts["host1"].Running; len(got) != 2 {
		t.Fatalf("after edit: %v", got)
	}
	write("hosts: [this is not a map\n", t0.Add(2*time.Minute)) // a bad edit keeps the last good list
	if got := srv.currentExpected().Hosts["host1"].Running; len(got) != 2 {
		t.Fatalf("after a bad edit: %v", got)
	}
}

// A demo visitor sees what an admin sees and changes nothing: every
// request that isn't a read is refused before its handler, but signing out
// (docs/demo.md).
func TestDemoSignin(t *testing.T) {
	srv, _ := testServer(t, false)
	h := srv.Handler()
	if rec := post(h, "/auth/demo", "", nil, map[string]string{"X-Beacon": "1"}); rec.Code != http.StatusNotFound {
		t.Fatalf("demo sign-in off: %d", rec.Code)
	}
	srv.cfg.DemoSignin = true
	if rec := get(h, "/api/v1/session", nil); !strings.Contains(rec.Body.String(), `"demo":true`) {
		t.Errorf("sign-in options don't offer the demo: %s", rec.Body)
	}
	if rec := post(h, "/auth/demo", "", nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("no X-Beacon: %d", rec.Code)
	}
	cookie := post(h, "/auth/demo", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	rec := get(h, "/api/v1/session", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":"admin"`) || !strings.Contains(rec.Body.String(), `"demo":true`) ||
		!strings.Contains(rec.Body.String(), `"user":"Demo visitor"`) {
		t.Fatalf("session: %d %s", rec.Code, rec.Body)
	}
	// Setup can be read, as an admin reads it.
	if rec := get(h, "/api/v1/settings/checks", cookie); rec.Code != http.StatusOK {
		t.Errorf("reading setup: %d %s", rec.Code, rec.Body)
	}
	// The people who run the demo stay private: stand-ins on Users, an
	// empty audit log.
	if rec := get(h, "/api/v1/users", cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "avery@example.com") {
		t.Errorf("users: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/audit", cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("audit: %d %s", rec.Code, rec.Body)
	}
	for _, w := range [][2]string{{"POST", "/api/v1/settings/checks/import"}, {"PUT", "/api/v1/settings/checks/x"},
		{"DELETE", "/api/v1/settings/channels/x"}, {"PUT", "/api/v1/incidents/1/note"}, {"POST", "/api/v1/agents/join"}} {
		req := httptest.NewRequest(w[0], w[1], strings.NewReader(`{}`))
		req.Header.Set("X-Beacon", "1")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "This is the demo") {
			t.Errorf("%s %s: %d %s", w[0], w[1], rec.Code, rec.Body)
		}
	}
	if rec := post(h, "/auth/signout", "", nil, map[string]string{"X-Beacon": "1", "Cookie": sessionCookie + "=" + cookie.Value}); rec.Code != http.StatusNoContent {
		t.Errorf("sign out: %d", rec.Code)
	}
	if rec := get(h, "/api/v1/session", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("after signing out: %d", rec.Code)
	}
	// Turned off, a visitor's session ends.
	cookie = post(h, "/auth/demo", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	srv.cfg.DemoSignin = false
	if rec := get(h, "/api/v1/overview", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("demo off, visitor still in: %d", rec.Code)
	}
}
