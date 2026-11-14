package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/vault"
)

// fakeVault is a three-server Raft cluster with one server unhealthy, a
// token that can't be renewed, 5 days from its end, and a PKI mount whose
// one certificate ends in 3 days.
func fakeVault(t *testing.T) (url, fingerprint string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "web.example.org"}, DNSNames: []string{"web.example.org"},
		NotBefore: time.Now().Add(-87 * 24 * time.Hour), NotAfter: time.Now().Add(3*24*time.Hour + time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	leaf := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch r.URL.Path {
		case "/v1/sys/mounts":
			v = map[string]any{"data": map[string]any{"pki/": map[string]any{"type": "pki"}}}
		case "/v1/pki/certs":
			v = map[string]any{"data": map[string]any{"keys": []string{"07"}}}
		case "/v1/pki/cert/07":
			v = map[string]any{"data": map[string]any{"certificate": leaf}}
		case "/v1/sys/health":
			v = map[string]any{"initialized": true, "sealed": false, "version": "1.21.4", "cluster_name": "vault-prod"}
		case "/v1/sys/leader":
			v = map[string]any{"ha_enabled": true, "is_self": true, "leader_address": "https://10.0.0.1:8200"}
		case "/v1/sys/storage/raft/autopilot/state":
			v = map[string]any{"data": map[string]any{"healthy": false, "failure_tolerance": 0, "voters": []string{"v1", "v2", "v3"}, "servers": map[string]any{
				"v1": map[string]any{"id": "v1", "name": "vault-1", "node_status": "alive", "healthy": true, "status": "leader"},
				"v2": map[string]any{"id": "v2", "name": "vault-2", "node_status": "alive", "healthy": true, "status": "voter"},
				"v3": map[string]any{"id": "v3", "name": "vault-3", "node_status": "alive", "healthy": false, "status": "voter", "last_contact": "3m2s"},
			}}}
		case "/v1/auth/token/lookup-self":
			v = map[string]any{"data": map[string]any{"display_name": "token-obs", "ttl": 5 * 86400, "creation_ttl": 30 * 86400, "renewable": false}}
		default:
			w.WriteHeader(http.StatusNotFound)
			v = map[string]any{"errors": []string{}}
		}
		_ = json.NewEncoder(w).Encode(v)
	})
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv.URL, hex.EncodeToString(sum[:])
}

// obs-beacond reads a Vault cluster itself; every copy shows it, judged
// as a cluster is, and it's forgotten when taken out of obs.yaml.
func TestVaultSource(t *testing.T) {
	a, b, _, _ := clusterCopies(t)
	url, fp := fakeVault(t)
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("hvs.obs"), 0o600)
	cfg := vault.Config{Name: "vault-prod", URL: url, TokenFile: tok, Fingerprint: fp}
	cfg.Defaults()
	a.cfg.Vault, b.cfg.Vault = []vault.Config{cfg}, []vault.Config{cfg}
	c, err := vault.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollVault(ctx, c); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	var v *ClusterView
	for v == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		b.syncWorkloads(false)
		for _, cv := range b.clusterViews(time.Now(), nil) {
			if cv.Name == "obs-beacond:vault-prod" && cv.Nodes == 3 {
				v = &cv
			}
		}
	}
	cancel()
	<-done
	if v == nil {
		t.Fatal("the other copy never showed it")
	}
	if v.Noun != "Vault" || v.NodeNoun != "Server" || v.NodesReady != 2 || v.Version != "Vault 1.21.4" || v.ReadBy != "" && v.ReadBy != serverSource {
		t.Errorf("on Clusters: %+v", v)
	}
	titles, sev := map[string]string{}, map[string]string{}
	for _, c := range b.workloadConditions(time.Now()) {
		titles[c.Key], sev[c.Key] = c.Title, c.Severity
	}
	certID := ""
	for k := range titles {
		if id, ok := strings.CutPrefix(k, "cert/obs-beacond:vault-prod/"); ok {
			certID = id
		}
	}
	if !aboutWorkloads("cert/obs-beacond:vault-prod/"+certID, "obs-beacond:vault-prod") {
		t.Error("a certificate's condition isn't counted on Clusters")
	}
	if sev["cert/obs-beacond:vault-prod/"+certID] != "critical" || sev["cluster/obs-beacond:vault-prod/"+vault.TokenPart] != "critical" ||
		sev["cluster/obs-beacond:vault-prod/cluster"] != "warning" {
		t.Errorf("severities %v", sev)
	}
	for key, want := range map[string]string{
		"cluster/obs-beacond:vault-prod/cluster":            "Vault vault-prod (obs-beacond): its Raft isn't healthy: vault-3; it can't lose another server and keep answering",
		"cluster/obs-beacond:vault-prod/" + vault.TokenPart: "Its monitoring token in vault-prod (obs-beacond): ends in 5 days (" + time.Now().Add(5*24*time.Hour).UTC().Format("2006-01-02") + "); it can't be renewed: make a new one before then",
		"node/obs-beacond:vault-prod/vault-3":               "",
		"cert/obs-beacond:vault-prod/" + certID:             "Certificate web.example.org from pki in vault-prod (obs-beacond): ends in 3 days (" + time.Now().Add(3*24*time.Hour+time.Hour).UTC().Format("2006-01-02") + ")",
	} {
		if want != "" && titles[key] != want {
			t.Errorf("%s: %q, want %q (all: %v)", key, titles[key], want, titles)
		}
		if want == "" && titles[key] == "" {
			t.Errorf("%s: no condition (all: %v)", key, titles)
		}
	}

	// Taken out of obs.yaml: forgotten.
	a.cfg.Vault, b.cfg.Vault = nil, nil
	a.forgetServerSources()
	b.syncWorkloads(true)
	for _, cv := range b.clusterViews(time.Now(), nil) {
		if cv.Name == "obs-beacond:vault-prod" {
			t.Errorf("still shown: %+v", cv)
		}
	}
	_ = report.KindCluster
}

func TestVaultConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("x"), 0o600)
	for name, body := range map[string]string{
		"twice":    "vault:\n  - {url: 'https://v1:8200', token_file: " + tok + ", name: v}\n  - {url: 'https://v2:8200', token_file: " + tok + ", name: v}\n",
		"http":     "vault:\n  - {url: 'http://v1:8200', token_file: " + tok + "}\n",
		"no token": "vault:\n  - {url: 'https://v1:8200'}\n",
		"reserved": "agents:\n  - {name: obs-beacond, token_sha256: " + HashToken(NewAgentToken()) + "}\n",
	} {
		p := filepath.Join(dir, name+".yaml")
		_ = os.WriteFile(p, []byte("listen: 127.0.0.1:0\n"+body), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := filepath.Join(dir, "good.yaml")
	_ = os.WriteFile(p, []byte("listen: 127.0.0.1:0\nvault:\n  - {url: 'https://Vault.example.org:8200', token_file: "+tok+"}\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil || len(c.Vault) != 1 || c.Vault[0].Name != "vault.example.org" || c.Vault[0].Every != time.Minute {
		t.Errorf("%v %+v", err, c)
	}
}
