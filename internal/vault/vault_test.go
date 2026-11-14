package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// policy is the monitoring policy docs/vault.md gives.
const policy = `
path "sys/storage/raft/autopilot/state" { capabilities = ["read"] }
path "sys/license/status"               { capabilities = ["read"] }
path "sys/metrics"                      { capabilities = ["read"] }
path "sys/mounts"                       { capabilities = ["read"] }
path "+/issuers"                        { capabilities = ["list"] }
path "+/issuer/+"                       { capabilities = ["read"] }
path "+/certs"                          { capabilities = ["list"] }
path "+/certs/revoked"                  { capabilities = ["list"] }
path "+/cert/+"                         { capabilities = ["read"] }
path "auth/token/renew-self"            { capabilities = ["update"] }
`

// rootCall asks a dev-mode server as root, to set it up.
func rootCall(t *testing.T, base, root, method, path string, body any, v any) {
	t.Helper()
	c, _ := newClient(Config{Name: "setup", URL: base, TokenFile: writeToken(t, root)})
	if _, err := c.call(context.Background(), method, path, true, body, v); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
}

func writeToken(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRealServers reads the dev-mode Vault and OpenBao the CI job runs
// (CAIRN_OBS_TEST_VAULT: their addresses, comma-separated; the root
// token is root-ci), with a token made from the documented policy.
// CAIRN_OBS_TEST_VAULT_RAFT is a Raft server's address and root token
// ("http://127.0.0.1:58202 hvs.…"), for a check by hand.
func TestRealServers(t *testing.T) {
	addrs := os.Getenv("CAIRN_OBS_TEST_VAULT")
	if addrs == "" {
		t.Skip("CAIRN_OBS_TEST_VAULT not set")
	}
	for _, base := range strings.Split(addrs, ",") {
		t.Run(base, func(t *testing.T) {
			rootCall(t, base, "root-ci", http.MethodPut, "sys/policies/acl/obs-monitor", map[string]any{"policy": policy}, nil)
			var made struct {
				Auth struct {
					ClientToken string `json:"client_token"`
				} `json:"auth"`
			}
			rootCall(t, base, "root-ci", http.MethodPost, "auth/token/create",
				map[string]any{"policies": []string{"obs-monitor"}, "ttl": "1h", "renewable": true, "display_name": "obs"}, &made)
			c, err := newClient(Config{Name: "dev", URL: base, TokenFile: writeToken(t, made.Auth.ClientToken)})
			if err != nil {
				t.Fatal(err)
			}
			s, err := c.Read(context.Background(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !s.Health.Initialized || s.Health.Sealed || s.Health.Version == "" || s.Raft != nil || s.RaftErr != "" || len(s.Problems) != 0 || s.Token == nil {
				t.Fatalf("%+v (token %+v, raft %q)", s, s.Token, s.RaftErr)
			}
			// An hour's token is renewed at once (under three weeks left),
			// and then it's fine: it renews itself in full.
			if !s.Renewed || s.RenewErr != "" {
				t.Errorf("renewed %v: %s", s.Renewed, s.RenewErr)
			}
			recs := Records(s)
			if len(recs) < 2 || recs[0].State != report.StateOK || recs[0].Name != s.Health.ClusterName || recs[1].Name != TokenPart || recs[1].State != report.StateOK ||
				!strings.HasPrefix(recs[1].Word, "Ends in 0 days") && !strings.HasPrefix(recs[1].Word, "Ends in 1 hours") {
				t.Errorf("records %+v", recs)
			}
			if want := map[string]string{"1.": "Vault", "2.": "OpenBao"}[s.Health.Version[:2]]; recs[0].Facts["distribution"] != want {
				t.Errorf("distribution %v for %s", recs[0].Facts["distribution"], s.Health.Version)
			}
			// A token that can't be renewed is judged by its days.
			rootCall(t, base, "root-ci", http.MethodPost, "auth/token/create",
				map[string]any{"policies": []string{"obs-monitor"}, "ttl": "1h", "renewable": false}, &made)
			c, _ = newClient(Config{Name: "dev", URL: base, TokenFile: writeToken(t, made.Auth.ClientToken)})
			s, _ = c.Read(context.Background(), time.Now())
			if tr := Records(s)[1]; s.Renewed || tr.State != report.StateDown || !strings.Contains(tr.Word, "it can't be renewed") {
				t.Errorf("not renewable: %+v", tr)
			}
			// A token Vault doesn't know.
			c, _ = newClient(Config{Name: "dev", URL: base, TokenFile: writeToken(t, "hvs.nonsense")})
			s, _ = c.Read(context.Background(), time.Now())
			if tr := Records(s)[1]; tr.State != report.StateDown || !strings.HasPrefix(tr.Word, "Refused: 403") {
				t.Errorf("unknown token: %+v", tr)
			}
		})
	}
	if raft := strings.Fields(os.Getenv("CAIRN_OBS_TEST_VAULT_RAFT")); len(raft) == 2 {
		c, _ := newClient(Config{Name: "raft", URL: raft[0], TokenFile: writeToken(t, raft[1])})
		s, err := c.Read(context.Background(), time.Now())
		if err != nil || s.Raft == nil || len(s.Raft.Servers) == 0 || s.Leader == nil || !s.Leader.HAEnabled {
			t.Fatalf("raft: %v %+v", err, s)
		}
		recs := Records(s)
		if recs[0].State != report.StateOK || recs[0].Facts["storage"] != "raft" || recs[1].Kind != report.KindNode || recs[1].Word != "Leader" {
			t.Errorf("raft records %+v", recs)
		}
	}
}

// fake answers as a Vault cluster does, from what's set: what dev mode
// can't show (sealed, Raft in trouble).
type fake struct {
	mu        sync.Mutex
	health    map[string]any
	code      int
	seal      map[string]any
	leader    map[string]any
	autopilot map[string]any
	token     map[string]any
	calls     []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	answer := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch r.URL.Path {
	case "/v1/sys/health":
		answer(f.code, f.health)
	case "/v1/sys/seal-status":
		answer(200, f.seal)
	case "/v1/sys/leader":
		answer(200, f.leader)
	case "/v1/sys/storage/raft/autopilot/state":
		answer(200, map[string]any{"data": f.autopilot})
	case "/v1/auth/token/lookup-self":
		answer(200, map[string]any{"data": f.token})
	default:
		answer(404, map[string]any{"errors": []string{}})
	}
}

func cluster() *fake {
	return &fake{code: 200,
		health: map[string]any{"initialized": true, "sealed": false, "version": "1.21.4+ent", "cluster_name": "vault-prod"},
		leader: map[string]any{"ha_enabled": true, "is_self": true, "leader_address": "https://10.0.0.1:8200"},
		autopilot: map[string]any{"healthy": true, "failure_tolerance": 1, "leader": "v1", "voters": []string{"v1", "v2", "v3"},
			"servers": map[string]any{
				"v1": map[string]any{"id": "v1", "name": "vault-1", "node_status": "alive", "healthy": true, "status": "leader", "last_contact": "0s"},
				"v2": map[string]any{"id": "v2", "name": "vault-2", "node_status": "alive", "healthy": true, "status": "voter", "last_contact": "210ms"},
				"v3": map[string]any{"id": "v3", "name": "vault-3", "node_status": "alive", "healthy": true, "status": "voter", "last_contact": "180ms"},
			}},
		token: map[string]any{"display_name": "token-obs", "policies": []string{"default", "obs-monitor"}, "ttl": 40 * 86400, "creation_ttl": 60 * 86400, "renewable": false},
	}
}

func readFake(t *testing.T, f *fake) (*Snapshot, map[string]report.Workload) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := newClient(Config{Name: "vault.example.org", URL: srv.URL, TokenFile: writeToken(t, "hvs.x")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]report.Workload{}
	for _, r := range Records(s) {
		m[r.ID] = r
	}
	return s, m
}

func TestStates(t *testing.T) {
	_, m := readFake(t, cluster())
	if c := m[report.ClusterSelf]; c.State != report.StateOK || c.Name != "vault-prod" || c.Facts["distribution"] != "Vault Enterprise" || c.Facts["voters"] != 3 {
		t.Errorf("healthy: %+v", c)
	}
	if n := m["raft/v2"]; n.Kind != report.KindNode || n.Name != "vault-2" || n.Word != "Voter" {
		t.Errorf("a voter: %+v", n)
	}
	if tk := m["token"]; tk.State != report.StateOK || !strings.HasPrefix(tk.Word, "Ends in 40 days") {
		t.Errorf("token: %+v", tk)
	}

	// One server down: Raft unhealthy, and no room for another.
	f := cluster()
	f.autopilot["healthy"], f.autopilot["failure_tolerance"] = false, 0
	f.autopilot["servers"].(map[string]any)["v3"] = map[string]any{"id": "v3", "name": "vault-3", "node_status": "alive", "healthy": false, "status": "voter", "last_contact": "4m12s"}
	f.token["ttl"] = 12 * 86400
	_, m = readFake(t, f)
	if c := m[report.ClusterSelf]; c.State != report.StateDegraded || c.Word != "Its Raft isn't healthy: vault-3; it can't lose another server and keep answering" {
		t.Errorf("a server down: %+v", c)
	}
	if n := m["raft/v3"]; n.State != report.StateDegraded || n.Word != "Unhealthy: last heard from 4m12s ago" {
		t.Errorf("the server: %+v", n)
	}
	if tk := m["token"]; tk.State != report.StateDegraded || !strings.Contains(tk.Word, "it can't be renewed") {
		t.Errorf("12 days left: %+v", tk)
	}

	// No leader.
	f = cluster()
	f.leader["leader_address"] = ""
	_, m = readFake(t, f)
	if c := m[report.ClusterSelf]; c.State != report.StateDown || !strings.HasPrefix(c.Word, "No leader") {
		t.Errorf("no leader: %+v", c)
	}

	// Sealed: nothing else is asked.
	f = cluster()
	f.code, f.health["sealed"] = 503, true
	f.seal = map[string]any{"type": "shamir", "t": 3, "progress": 1}
	s, m := readFake(t, f)
	if c := m[report.ClusterSelf]; len(m) != 1 || c.State != report.StateDown || c.Word != "Sealed: nothing can read a secret until it's unsealed (1 of 3 unseal keys entered)" {
		t.Errorf("sealed: %+v", m)
	}
	for _, call := range f.calls {
		if call != "GET /v1/sys/health" && call != "GET /v1/sys/seal-status" {
			t.Errorf("sealed, but asked %s (%v)", call, s)
		}
	}

	// OpenBao, not initialized.
	f = cluster()
	f.code, f.health["initialized"], f.health["version"] = 501, false, "2.4.1"
	_, m = readFake(t, f)
	if c := m[report.ClusterSelf]; c.State != report.StateDown || c.Facts["distribution"] != "OpenBao" || !strings.HasPrefix(c.Word, "Not initialized") {
		t.Errorf("not initialized: %+v", c)
	}
}

func TestTokenFileRotates(t *testing.T) {
	p := writeToken(t, "hvs.first")
	c, _ := newClient(Config{Name: "v", URL: "http://127.0.0.1:1", TokenFile: p})
	if tok, _ := c.readToken(); tok != "hvs.first" {
		t.Fatal(tok)
	}
	_ = os.WriteFile(p, []byte("hvs.second\n"), 0o600)
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(p, later, later)
	if tok, _ := c.readToken(); tok != "hvs.second" {
		t.Errorf("after rotating: %s", tok)
	}
}

func TestConfig(t *testing.T) {
	tok := writeToken(t, "x")
	for _, cfg := range []Config{
		{URL: "http://vault:8200", TokenFile: tok},
		{URL: "https://vault:8200"},
		{URL: "https://vault:8200", TokenFile: tok, Every: time.Second},
		{URL: "https://vault:8200", TokenFile: tok, Fingerprint: "abc"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	c := Config{URL: "https://Vault.Example.org:8200", TokenFile: tok}
	c.Defaults()
	if c.Name != "vault.example.org" || c.Every != time.Minute || c.MinLifetime != 7*24*time.Hour {
		t.Errorf("%+v", c)
	}
	if _, err := New(c); err != nil {
		t.Error(err)
	}
}

func TestUnreachable(t *testing.T) {
	_, m := readFake(t, cluster())
	var last []report.Workload
	for _, r := range m {
		last = append(last, r)
	}
	down := Unreachable("vault.example.org", last, os.ErrDeadlineExceeded)
	if len(down) != len(last) || down[0].State != report.StateDown || down[0].Name != "vault-prod" || !strings.HasPrefix(down[0].Word, "Can't reach its API") {
		t.Errorf("%+v", down[0])
	}
}
