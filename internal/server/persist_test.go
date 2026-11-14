package server

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// stateServer is testServer with a database in dir; calling it twice with
// the same dir is a restart.
func stateServer(t *testing.T, dir string, dev bool) *Server {
	t.Helper()
	key := filepath.Join(dir, "state.key")
	if _, err := os.Stat(key); err != nil {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		_ = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(b)), 0o600)
	}
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: dev, StaleAfter: time.Minute,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []AgentConfig{{Name: "host1", TokenSHA256: HashToken(NewAgentToken())}}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	cookie := post(srv.Handler(), "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	srv.Close()

	srv = stateServer(t, dir, true)
	if rec := get(srv.Handler(), "/api/v1/session", cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Developer (local)") {
		t.Fatalf("after restart: %d %s", rec.Code, rec.Body)
	}
	// The cookie itself is not in the database, only its SHA-256.
	srv.Close()
	if statetest.Postgres() {
		return
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "beacon.db"))
	wal, _ := os.ReadFile(filepath.Join(dir, "beacon.db-wal"))
	if strings.Contains(string(raw)+string(wal), cookie.Value) {
		t.Fatal("the database holds the session cookie")
	}

	// Turning dev sign-in off ends the stored dev session.
	srv = stateServer(t, dir, false)
	if rec := get(srv.Handler(), "/api/v1/session", cookie); rec.Code != 401 {
		t.Fatalf("dev session outlived dev sign-in: %d", rec.Code)
	}
}

func TestSignOutSticksAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	if rec := postCookie(h, "/auth/signout", cookie); rec.Code >= 400 {
		t.Fatalf("sign out: %d", rec.Code)
	}
	srv.Close()
	srv = stateServer(t, dir, true)
	if rec := get(srv.Handler(), "/api/v1/session", cookie); rec.Code != 401 {
		t.Fatalf("signed-out session came back: %d", rec.Code)
	}
}

func TestIncidentsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, false)
	t0 := time.Now()
	srv.alerts.evaluate(t0, []Condition{demoDown})
	srv.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown})
	srv.alerts.acknowledge("B-0001", "Pat Admin", t0.Add(4*time.Minute), true)
	srv.Close()

	srv = stateServer(t, dir, false)
	open, _ := srv.alerts.snapshot()
	if len(open) != 1 || open[0].ID != "B-0001" || open[0].Fired == nil || open[0].Ack == nil || open[0].Ack.By != "Pat Admin" {
		t.Fatalf("after restart: %+v", open)
	}
	// Still the same incident: fixing it now resolves B-0001 rather than
	// leaving it open forever, and the next one gets a new number.
	srv.alerts.evaluate(t0.Add(5*time.Minute), nil)
	if open, recent := srv.alerts.snapshot(); len(open) != 0 || len(recent) != 1 || recent[0].Resolved == nil {
		t.Fatalf("resolve after restart: open=%+v recent=%+v", open, recent)
	}
	srv.alerts.evaluate(t0.Add(6*time.Minute), []Condition{demoDown})
	srv.alerts.evaluate(t0.Add(9*time.Minute), []Condition{demoDown})
	if open, _ := srv.alerts.snapshot(); len(open) != 1 || open[0].ID != "B-0002" {
		t.Fatalf("numbering after restart: %+v", open)
	}
}

func TestAuditAPI(t *testing.T) {
	srv := stateServer(t, t.TempDir(), true)
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	rec := get(h, "/api/v1/audit", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"action":"signed in"`) || !strings.Contains(rec.Body.String(), `"stored":true`) {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/audit", nil); rec.Code != 401 {
		t.Fatalf("audit without a session: %d", rec.Code)
	}
}

func postCookie(h http.Handler, path string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, nil)
	req.AddCookie(c)
	req.Header.Set("X-Beacon", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRestartDoesNotResolveBeforeReports(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, false)
	t0 := time.Now()
	srv.alerts.evaluate(t0, []Condition{demoDown})
	srv.alerts.evaluate(t0.Add(3*time.Minute), []Condition{demoDown})
	srv.Close()

	srv = stateServer(t, dir, false)
	srv.evaluate(srv.started.Add(20 * time.Second)) // no reports yet
	if open, _ := srv.alerts.snapshot(); len(open) != 1 {
		t.Fatal("an incident brought back resolved before any agent reported")
	}
}
