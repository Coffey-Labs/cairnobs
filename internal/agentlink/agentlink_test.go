package agentlink_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/agentlink"
	"github.com/cairnobs/cairnobs/internal/jointoken"
	"github.com/cairnobs/cairnobs/internal/pki"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/server"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// rig is obs-beacond's agent listener over TLS, with a database in dir.
// Building it again on the same dir is a restart.
type rig struct {
	cfg   *server.Config
	srv   *server.Server
	ts    *httptest.Server
	pin   string
	token string // node1's
	tokf  string // a file holding it
}

func newRig(t *testing.T, dir, token string) *rig {
	t.Helper()
	key := filepath.Join(dir, "state.key")
	if _, err := os.Stat(key); err != nil {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(b)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &server.Config{Listen: "127.0.0.1:0", AgentListen: "127.0.0.1:0", StaleAfter: time.Minute,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []server.AgentConfig{{Name: "node1", TokenSHA256: server.HashToken(token)}}}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Not StartTLS: it adds httptest's own certificate, which Go serves
	// ahead of GetCertificate to clients that send no server name (anyone
	// connecting to an IP). The real listener has only GetCertificate.
	ts := httptest.NewUnstartedServer(srv.AgentHandler())
	ts.Listener = tls.NewListener(ts.Listener, srv.AgentTLSConfig())
	ts.Start()
	ts.URL = "https://" + ts.Listener.Addr().String()
	t.Cleanup(func() { ts.Close(); srv.Close() })

	b, err := os.ReadFile(server.AgentCAFile(cfg))
	if err != nil {
		t.Fatalf("obs-beacond didn't write its CA: %v", err)
	}
	blk, _ := pem.Decode(b)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tokf := filepath.Join(dir, "agent.token")
	if err := os.WriteFile(tokf, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &rig{cfg: cfg, srv: srv, ts: ts, pin: pki.Fingerprint(ca), token: token, tokf: tokf}
}

func (r *rig) link(t *testing.T, stateDir string) *agentlink.Link {
	t.Helper()
	l, err := agentlink.New(agentlink.Config{Server: r.ts.URL, CAFingerprint: r.pin, TokenFile: r.tokf, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// report sends one report the way the agent does and returns the status.
func reportWith(t *testing.T, l *agentlink.Link, url string, hdr map[string]string) int {
	t.Helper()
	body, _ := json.Marshal(report.Report{V: report.Version, Host: "node1", Time: time.Now().UTC(), Interval: 15})
	req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/agent/report", bytes.NewReader(body))
	l.Authorize(req)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := l.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestEnrollReportRenew(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	state := t.TempDir()
	l := r.link(t, state)
	if l.Ready() {
		t.Fatal("ready before enrolling")
	}
	if err := l.Maintain(ctx, time.Now()); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !l.Ready() {
		t.Fatal("not ready after enrolling")
	}
	if got := reportWith(t, l, r.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("report with the certificate: %d", got)
	}
	for _, f := range []string{"agent.key", "agent.crt", "ca.pem"} {
		st, err := os.Stat(filepath.Join(state, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "agent.key" && st.Mode().Perm() != 0o600 {
			t.Errorf("agent.key is %v, want 0600", st.Mode().Perm())
		}
	}

	// Nothing to do yet; past two thirds of the life, a renewal.
	first := l.NotAfter()
	if err := l.Maintain(ctx, time.Now()); err != nil || !l.NotAfter().Equal(first) {
		t.Fatalf("renewed too early: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // certificate times are whole seconds
	if err := l.Maintain(ctx, time.Now().Add(pki.AgentLifetime*2/3+time.Hour)); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !l.NotAfter().After(first) {
		t.Fatal("renewal didn't give a new certificate")
	}
	if got := reportWith(t, l, r.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("report after renewing: %d", got)
	}
}

func TestTokenOnlyEnrolls(t *testing.T) {
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	l := r.link(t, t.TempDir()) // never enrolled: no certificate
	if got := reportWith(t, l, r.ts.URL, map[string]string{"Authorization": "Bearer " + r.token}); got != http.StatusUnauthorized {
		t.Fatalf("report with only the token: %d, want 401", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.token")
	_ = os.WriteFile(bad, []byte(server.NewAgentToken()), 0o600)
	l2, _ := agentlink.New(agentlink.Config{Server: r.ts.URL, CAFingerprint: r.pin, TokenFile: bad, StateDir: t.TempDir()})
	if err := l2.Maintain(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("enrolling with an unknown token: %v", err)
	}
}

func TestWrongPin(t *testing.T) {
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	other, _ := pki.NewCA(time.Now())
	l, err := agentlink.New(agentlink.Config{Server: r.ts.URL, CAFingerprint: other.Fingerprint(), TokenFile: r.tokf, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Maintain(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "not the pinned one") {
		t.Fatalf("enrolling against the wrong CA: %v", err)
	}
	if _, err := agentlink.New(agentlink.Config{Server: r.ts.URL, TokenFile: r.tokf, StateDir: t.TempDir()}); err == nil {
		t.Fatal("https without ca_fingerprint was accepted")
	}
}

func TestEnrollingAgainRevokesTheOld(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	a, b := r.link(t, t.TempDir()), r.link(t, t.TempDir())
	if err := a.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := reportWith(t, a, r.ts.URL, nil); got != http.StatusUnauthorized {
		t.Fatalf("the older certificate still works: %d", got)
	}
	// The agent's answer to a 401: drop it, enroll again with the token.
	a.Refused()
	if a.Ready() {
		t.Fatal("still ready after a refusal")
	}
	if err := a.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := reportWith(t, a, r.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("after enrolling again: %d", got)
	}
}

func TestRemovedAgentIsCutOff(t *testing.T) {
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	l := r.link(t, t.TempDir())
	if err := l.Maintain(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.cfg.Agents = nil // as if removed from obs-beacond.yaml and restarted
	if got := reportWith(t, l, r.ts.URL, nil); got != http.StatusUnauthorized {
		t.Fatalf("a removed agent's certificate still works: %d", got)
	}
}

func TestRestarts(t *testing.T) {
	ctx := context.Background()
	dir, state := t.TempDir(), t.TempDir()
	token := server.NewAgentToken()
	r := newRig(t, dir, token)
	if err := r.link(t, state).Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.ts.Close()
	r.srv.Close()

	// obs-beacond restarts: same CA. The agent restarts: its certificate
	// comes back from disk, no enrolling.
	r2 := newRig(t, dir, token)
	if r2.pin != r.pin {
		t.Fatal("the agent CA changed across a restart")
	}
	_ = os.Remove(r2.tokf) // prove the token isn't used
	l := r2.link(t, state)
	if !l.Ready() {
		t.Fatal("the kept certificate wasn't loaded")
	}
	if got := reportWith(t, l, r2.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("report after both restarts: %d", got)
	}

	// A different state key can't open the CA: refuse to start rather
	// than make a new CA and strand every agent.
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	_ = os.WriteFile(filepath.Join(dir, "state.key"), []byte(base64.StdEncoding.EncodeToString(b)), 0o600)
	cfg := *r2.cfg
	if _, err := server.New(&cfg); err == nil || !strings.Contains(err.Error(), "unsealed") {
		t.Fatalf("started with the wrong state key: %v", err)
	}
}

func TestTLSNeedsADatabase(t *testing.T) {
	cfg := &server.Config{Listen: "127.0.0.1:0", AgentListen: "127.0.0.1:0", StaleAfter: time.Minute}
	if _, err := server.New(cfg); err == nil {
		t.Fatal("TLS agent listener without a database was accepted")
	}
	cfg.AgentTLS.Off = true
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.AgentTLSConfig() != nil {
		t.Fatal("agent_tls.off still serves TLS")
	}
}

func TestPlainHTTPUsesTheToken(t *testing.T) {
	token := server.NewAgentToken()
	cfg := &server.Config{Listen: "127.0.0.1:0", AgentListen: "127.0.0.1:0", StaleAfter: time.Minute,
		Agents: []server.AgentConfig{{Name: "node1", TokenSHA256: server.HashToken(token)}}}
	cfg.AgentTLS.Off = true
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.AgentHandler())
	defer ts.Close()
	tokf := filepath.Join(t.TempDir(), "agent.token")
	_ = os.WriteFile(tokf, []byte(token), 0o600)
	l, err := agentlink.New(agentlink.Config{Server: ts.URL, TokenFile: tokf})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Ready() || l.TLS() {
		t.Fatal("a plain link should be ready and not TLS")
	}
	if got := reportWith(t, l, ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("plain report: %d", got)
	}
}

// admin signs in to the web listener (development sign-in) and returns a
// function that makes API calls as that admin.
func admin(t *testing.T, r *rig) func(method, path, body string) (int, map[string]any) {
	t.Helper()
	r.cfg.DevSignin = true
	h := r.srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/auth/dev", nil)
	req.Header.Set("X-Beacon", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("dev sign-in: %d %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	return func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Beacon", "1")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

func TestJoin(t *testing.T) {
	ctx := context.Background()
	dir, state := t.TempDir(), t.TempDir()
	token := server.NewAgentToken()
	r := newRig(t, dir, token)
	r.cfg.Join.AgentURL = r.ts.URL
	api := admin(t, r)

	code, out := api(http.MethodPost, "/api/v1/agents/join", `{"name":"web1"}`)
	if code != http.StatusOK {
		t.Fatalf("make a join token: %d %v", code, out)
	}
	cmd, _ := out["command"].(string)
	tokStr, _ := out["token"].(string)
	if !strings.Contains(cmd, "/install.sh | sudo sh -s -- --join "+tokStr) {
		t.Fatalf("command: %q", cmd)
	}
	tok, err := jointoken.Parse(tokStr)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Server != r.ts.URL || tok.CAFingerprint != r.pin {
		t.Fatalf("token carries %s %s", tok.Server, tok.CAFingerprint)
	}
	if code, _ := api(http.MethodPost, "/api/v1/agents/join", `{"name":"web1"}`); code != http.StatusConflict {
		t.Fatalf("a second token for a waiting name: %d", code)
	}
	if code, _ := api(http.MethodPost, "/api/v1/agents/join", `{"name":"node1"}`); code != http.StatusConflict {
		t.Fatalf("a token for a name in the config file: %d", code)
	}
	if code, _ := api(http.MethodPost, "/api/v1/agents/join", `{"name":"Not A Name"}`); code != http.StatusBadRequest {
		t.Fatalf("a bad name: %d", code)
	}

	name, err := agentlink.Join(ctx, tok.Server, tok.CAFingerprint, tok.Secret, state)
	if err != nil || name != "web1" {
		t.Fatalf("join: %q %v", name, err)
	}
	if _, err := agentlink.Join(ctx, tok.Server, tok.CAFingerprint, tok.Secret, t.TempDir()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("the token worked twice: %v", err)
	}
	other, _ := pki.NewCA(time.Now())
	if _, err := agentlink.Join(ctx, tok.Server, other.Fingerprint(), tok.Secret, t.TempDir()); err == nil {
		t.Fatal("joined a server whose CA isn't the token's")
	}

	l, err := agentlink.New(agentlink.Config{Server: tok.Server, CAFingerprint: tok.CAFingerprint, StateDir: state})
	if err != nil || !l.Ready() {
		t.Fatalf("the joined agent's link: %v", err)
	}
	if got := reportWith(t, l, r.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("report from the joined agent: %d", got)
	}
	code, out = api(http.MethodGet, "/api/v1/agents", "")
	if code != http.StatusOK || !strings.Contains(fmtJSON(out["agents"]), `"name":"web1","source":"joined"`) {
		t.Fatalf("agents: %d %s", code, fmtJSON(out))
	}

	// It survives obs-beacond restarting.
	r.ts.Close()
	r.srv.Close()
	r2 := newRig(t, dir, token)
	l2, _ := agentlink.New(agentlink.Config{Server: r2.ts.URL, CAFingerprint: r2.pin, StateDir: state})
	if got := reportWith(t, l2, r2.ts.URL, nil); got != http.StatusNoContent {
		t.Fatalf("report after a restart: %d", got)
	}

	// Removing it cuts it off; a config-file agent can't be removed here.
	api2 := admin(t, r2)
	if code, _ := api2(http.MethodDelete, "/api/v1/agents/node1", ""); code != http.StatusConflict {
		t.Fatalf("removed a config-file agent: %d", code)
	}
	if code, _ := api2(http.MethodDelete, "/api/v1/agents/web1", ""); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if got := reportWith(t, l2, r2.ts.URL, nil); got != http.StatusUnauthorized {
		t.Fatalf("a removed agent still reports: %d", got)
	}

	// A waiting token can be cancelled.
	r2.cfg.Join.AgentURL = r2.ts.URL
	_, out = api2(http.MethodPost, "/api/v1/agents/join", `{"name":"web2"}`)
	tok2, _ := jointoken.Parse(out["token"].(string))
	_, out = api2(http.MethodGet, "/api/v1/agents", "")
	pending, _ := out["pending"].([]any)
	if len(pending) != 1 {
		t.Fatalf("pending: %v", out["pending"])
	}
	id := pending[0].(map[string]any)["id"].(string)
	if code, _ := api2(http.MethodDelete, "/api/v1/agents/join/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("cancel: %d", code)
	}
	if _, err := agentlink.Join(ctx, tok2.Server, tok2.CAFingerprint, tok2.Secret, t.TempDir()); err == nil {
		t.Fatal("a cancelled token joined")
	}
}

func TestJoinNeedsTLS(t *testing.T) {
	cfg := &server.Config{Listen: "127.0.0.1:0", AgentListen: "127.0.0.1:0", StaleAfter: time.Minute, DevSignin: true}
	cfg.AgentTLS.Off = true
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	api := admin(t, &rig{cfg: cfg, srv: srv})
	code, out := api(http.MethodPost, "/api/v1/agents/join", `{"name":"web1"}`)
	if code != http.StatusConflict || !strings.Contains(fmtJSON(out), "database") {
		t.Fatalf("join without a database: %d %v", code, out)
	}
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// memStore keeps the key and certificate in memory, as the cluster agent's
// Secret does.
type memStore struct{ key, cert, ca []byte }

func (m *memStore) Load() (key, cert, ca []byte, err error) {
	if m.key == nil {
		return nil, nil, nil, os.ErrNotExist
	}
	return m.key, m.cert, m.ca, nil
}

func (m *memStore) Save(key, cert, ca []byte) error {
	m.key, m.cert, m.ca = key, cert, ca
	return nil
}

// A cluster joins as a host does, but from a manifest, and it's a cluster
// agent: it sends workloads, not host reports, and isn't listed as a host.
func TestJoinACluster(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, t.TempDir(), server.NewAgentToken())
	r.cfg.Join.AgentURL = r.ts.URL
	api := admin(t, r)
	if code, out := api(http.MethodPost, "/api/v1/agents/join", `{"name":"prod","cluster":true}`); code != http.StatusConflict ||
		!strings.Contains(fmtJSON(out), "join.image") {
		t.Fatalf("a cluster without join.image: %d %v", code, out)
	}
	r.cfg.Join.Image = "registry.example.org/ops/obs-beacond"
	code, out := api(http.MethodPost, "/api/v1/agents/join", `{"name":"prod","cluster":true}`)
	if code != http.StatusOK {
		t.Fatalf("make a cluster join token: %d %v", code, out)
	}
	tokStr := out["token"].(string)
	if cmd := out["command"].(string); !strings.Contains(cmd, "-H 'Authorization: Bearer "+tokStr+"'") || !strings.HasSuffix(cmd, "/cluster.yaml | kubectl apply -f -") {
		t.Fatalf("command: %q", cmd)
	}
	// The manifest: with the token, its Secret; without, the manifest alone.
	manifest := func(auth string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/cluster.yaml", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, m := manifest(tokStr)
	if code != http.StatusOK || !strings.Contains(m, "image: \"registry.example.org/ops/obs-beacond:") || !strings.Contains(m, "kind: Secret") ||
		!strings.Contains(m, "join: "+tokStr) || strings.Contains(m, "@IMAGE@") {
		t.Fatalf("manifest with the token: %d\n%s", code, m)
	}
	if strings.Index(m, "kind: Secret") > strings.Index(m, "kind: Deployment") || strings.Contains(m, "\n#@SECRET@") {
		t.Fatalf("the Secret goes before the Deployment that reads it, in the marker's place:\n%s", m)
	}
	if code, m := manifest(""); code != http.StatusOK || strings.Contains(m, "kind: Secret\n") || !strings.Contains(m, "kind: ClusterRole") || strings.Contains(m, "\n#@SECRET@") {
		t.Fatalf("manifest alone: %d", code)
	}
	if code, _ := manifest("cobsj1_nonsense"); code != http.StatusUnauthorized {
		t.Fatalf("a bad token: %d", code)
	}
	// A host's token isn't a cluster's.
	_, hostOut := api(http.MethodPost, "/api/v1/agents/join", `{"name":"web1"}`)
	if code, _ := manifest(hostOut["token"].(string)); code != http.StatusUnauthorized {
		t.Fatalf("a host's token for the manifest: %d", code)
	}

	tok, _ := jointoken.Parse(tokStr)
	st := &memStore{}
	cfg := agentlink.Config{Server: tok.Server, CAFingerprint: tok.CAFingerprint, Store: st}
	name, err := agentlink.JoinStore(ctx, cfg, tok.Secret)
	if err != nil || name != "prod" || st.cert == nil {
		t.Fatalf("join: %q %v", name, err)
	}
	if code, _ := manifest(tokStr); code != http.StatusUnauthorized {
		t.Fatalf("the manifest with a spent token: %d", code)
	}
	l, err := agentlink.New(cfg)
	if err != nil || !l.Ready() || l.Name() != "prod" {
		t.Fatalf("the cluster's link from its store: %v", err)
	}
	workloads := func() int {
		body, _ := json.Marshal(report.Workloads{V: report.Version, Generation: "g", Full: report.FullBegin,
			Records: []report.Workload{{Kind: report.KindCluster, ID: "cluster", Name: "prod", State: report.StateOK, Word: "API answering"}}})
		req, _ := http.NewRequest(http.MethodPost, r.ts.URL+"/api/v1/agent/workloads", bytes.NewReader(body))
		resp, err := l.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := workloads(); got != http.StatusNoContent {
		t.Fatalf("workloads from the joined cluster: %d", got)
	}
	if got := reportWith(t, l, r.ts.URL, nil); got != http.StatusForbidden {
		t.Fatalf("a host report from a cluster agent: %d", got)
	}
	_, out = api(http.MethodGet, "/api/v1/agents", "")
	if strings.Contains(fmtJSON(out["agents"]), `"prod"`) || !strings.Contains(fmtJSON(out["clusters"]), `"name":"prod","source":"joined"`) {
		t.Fatalf("listed: %s", fmtJSON(out))
	}
	_, out = api(http.MethodGet, "/api/v1/clusters", "")
	if !strings.Contains(fmtJSON(out), `"name":"prod"`) {
		t.Fatalf("clusters page: %s", fmtJSON(out))
	}
	// Removing it cuts it off and forgets what it sent.
	if code, _ := api(http.MethodDelete, "/api/v1/agents/prod", ""); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if got := workloads(); got != http.StatusUnauthorized {
		t.Fatalf("a removed cluster still sends: %d", got)
	}
	if _, out = api(http.MethodGet, "/api/v1/clusters", ""); strings.Contains(fmtJSON(out), `"name":"prod"`) {
		t.Fatalf("a removed cluster is still shown: %s", fmtJSON(out))
	}
}
