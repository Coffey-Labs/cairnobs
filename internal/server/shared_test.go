package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// Two copies of obs-beacond on one database, as behind a load balancer.
func twoCopies(t *testing.T, edit func(*Config)) (a, b *Server) {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	_ = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k)), 0o600)
	for _, s := range []**Server{&a, &b} {
		cfg := &Config{Listen: "127.0.0.1:0", StaleAfter: time.Minute, Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key}
		edit(cfg)
		srv, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { srv.Close() })
		*s = srv
	}
	return a, b
}

// A sign-in sent to the provider by one copy finishes on the other, once.
func TestSigninFinishesOnAnotherCopy(t *testing.T) {
	f := newFakeOIDC(t)
	f.claims["groups"] = []string{"obs-admins"}
	pc := oidcConfig(f)
	if err := pc.check(); err != nil {
		t.Fatal(err)
	}
	a, b := twoCopies(t, func(c *Config) { c.Signin = SigninConfig{Providers: []ProviderConfig{pc}} })
	rec := get(a.Handler(), "/auth/sso/login?return=/hosts", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d", rec.Code)
	}
	oauth := rec.Result().Cookies()[0]
	u, _ := url.Parse(f.approve(rec.Header().Get("Location")))
	back := func(srv *Server) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", u.Path+"?"+u.RawQuery, nil)
		req.AddCookie(oauth)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec = back(b)
	if loc := rec.Header().Get("Location"); loc != "/hosts" {
		t.Fatalf("on the other copy: %d %s", rec.Code, loc)
	}
	var signedIn bool
	for _, c := range rec.Result().Cookies() {
		signedIn = signedIn || (c.Name == sessionCookie && c.Value != "")
	}
	if !signedIn {
		t.Fatal("no session")
	}
	// The same redirect again, on either copy, is spent.
	for _, srv := range []*Server{a, b} {
		if loc := back(srv).Header().Get("Location"); !strings.Contains(loc, "signin=expired") {
			t.Fatalf("replayed: %s", loc)
		}
	}
}

// eventually waits for something another copy will hear of shortly.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: never happened", what)
}

// Copies on one PostgreSQL database show the same hosts, know each other's
// sessions and settings, and a sign-out on one ends the session on all.
func TestCopiesShareLiveState(t *testing.T) {
	if !statetest.Postgres() {
		t.Skipf("PostgreSQL only: set %s", statetest.Env)
	}
	tok := NewAgentToken()
	a, b := twoCopies(t, func(c *Config) {
		c.DevSignin = true
		c.Agents = []AgentConfig{{Name: "host1", TokenSHA256: HashToken(tok)}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.follow(ctx)
	select {
	case <-b.caughtUp:
	case <-time.After(10 * time.Second):
		t.Fatal("b never caught up")
	}

	// A report taken by a reaches b.
	body, _ := json.Marshal(report.Report{V: report.Version, Host: "host1", CPU: 42})
	if rec := post(a.AgentHandler(), "/api/v1/agent/report", tok, body, nil); rec.Code != 204 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	eventually(t, "b has host1's report", func() bool { return !b.store.Seen("host1").IsZero() })
	for _, h := range b.store.Snapshot() {
		if h.name == "host1" && (h.last.CPU != 42 || len(h.cpu) != 1) {
			t.Fatalf("b's host1: %+v", h)
		}
	}
	// A queued one adds to b's sparkline only.
	body, _ = json.Marshal(report.Report{V: report.Version, Host: "host1", CPU: 7})
	post(a.AgentHandler(), "/api/v1/agent/report", tok, body, map[string]string{"X-Beacon-Queued": "1"})
	eventually(t, "b has the queued point", func() bool {
		for _, h := range b.store.Snapshot() {
			if h.name == "host1" && len(h.cpu) == 2 && h.last.CPU == 42 {
				return true
			}
		}
		return false
	})

	// Signed in on a, known to b; signed out on a, gone from b.
	c := devCookie(t, a.Handler())
	if rec := get(b.Handler(), "/api/v1/session", c); rec.Code != 200 {
		t.Fatalf("session on b: %d", rec.Code)
	}
	post(a.Handler(), "/auth/signout", "", nil, map[string]string{"X-Beacon": "1", "Cookie": sessionCookie + "=" + c.Value})
	eventually(t, "b forgets the session", func() bool { return get(b.Handler(), "/api/v1/session", c).Code == 401 })

	// A maintenance window saved on a is in force on b.
	c = devCookie(t, a.Handler())
	if rec := sendCookie(a.Handler(), "PUT", "/api/v1/settings/maintenance/w1", c, `{"hosts":["host1"],"daily":"04:30","timezone":"UTC","duration_s":600}`); rec.Code != 200 {
		t.Fatalf("window: %d %s", rec.Code, rec.Body)
	}
	eventually(t, "b has the window", func() bool { return len(b.windows()) == 1 })
}
