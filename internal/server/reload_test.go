package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// Step 2 of docs/cert-rotation.md: a port still serving the old
// certificate is offered a reload only when its host's cert_reload lists
// a unit for the renewed file; an admin's press queues it once, audited.
func TestCertReloadOffer(t *testing.T) {
	srv, _ := actionServer(t)
	h := srv.Handler()
	c := devCookie(t, h)
	now := time.Now()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	old := testCert(t, "admin.internal", ec, &ec.PublicKey, 5*24*time.Hour)
	renewed := testCert(t, "admin.internal", ec, &ec.PublicKey, 85*24*time.Hour)
	r := report.Report{V: report.Version, Time: now,
		Certs:     &report.CertScan{At: now, List: []report.CertSeen{{Addr: "127.0.0.1", Port: 8443, Owner: "admin", How: "tls", Cert: *old, At: now}}},
		CertFiles: &report.CertFiles{At: now, List: []report.CertFile{{Path: "/etc/letsencrypt/live/admin/cert.pem", Mod: now.Add(-3 * time.Hour), Cert: *renewed}}},
	}
	type page struct {
		Certificates []CertInv `json:"certificates"`
		CanAct       bool      `json:"can_act"`
	}
	offers := func() []CertReload {
		var p page
		_ = json.Unmarshal(get(h, "/api/v1/certificates?status=problems", c).Body.Bytes(), &p)
		if !p.CanAct {
			t.Fatal("can_act false with a database")
		}
		for _, x := range p.Certificates {
			if x.Fingerprint == old.Fingerprint {
				return x.Reloads
			}
		}
		t.Fatal("no row for the old certificate")
		return nil
	}

	// No cert_reload on the host: nothing on offer, and nothing taken.
	srv.store.Put("node1", r, now)
	if o := offers(); len(o) != 0 {
		t.Fatalf("offered without cert_reload: %+v", o)
	}
	ask := `{"host":"node1","unit":"admin.service","addr":"127.0.0.1","port":8443}`
	if rec := sendCookie(h, "POST", "/api/v1/certificates/reload", c, ask); rec.Code != 409 {
		t.Fatalf("not on offer: %d %s", rec.Code, rec.Body)
	}

	// A rule for other files: still nothing.
	r.Reloads = []report.ReloadRule{{Unit: "nginx.service", Files: []string{"/etc/nginx/**/*.pem"}}}
	srv.store.Put("node1", r, now)
	if o := offers(); len(o) != 0 {
		t.Fatalf("offered for another unit's files: %+v", o)
	}

	r.Reloads = append(r.Reloads, report.ReloadRule{Unit: "admin.service", Files: []string{"/etc/letsencrypt/live/*/cert.pem"}})
	srv.store.Put("node1", r, now)
	o := offers()
	if len(o) != 1 || o[0].Unit != "admin.service" || o[0].Port != 8443 || o[0].Where != "node1:8443 · admin" || o[0].File != "/etc/letsencrypt/live/admin/cert.pem" || o[0].Last != nil {
		t.Fatalf("offer: %+v", o)
	}
	if rec := sendCookie(h, "POST", "/api/v1/certificates/reload", c, strings.Replace(ask, "admin.service", "sshd.service", 1)); rec.Code != 409 {
		t.Fatalf("another unit: %d %s", rec.Code, rec.Body)
	}
	rec := sendCookie(h, "POST", "/api/v1/certificates/reload", c, ask)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"`) {
		t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/certificates/reload", c, ask); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already waiting") {
		t.Fatalf("twice: %d %s", rec.Code, rec.Body)
	}
	if o := offers(); len(o) != 1 || o[0].Last == nil || o[0].Last.State != "waiting" || o[0].Last.Kind != "reload" {
		t.Fatalf("after asking: %+v", o)
	}
	// The agent gets the unit and the port, nothing else.
	as, _ := srv.db.Actions(10)
	if len(as) != 1 || as[0].Agent != "node1" || as[0].Args != `{"unit":"admin.service","addr":"127.0.0.1","port":8443}` {
		t.Fatalf("queued: %+v", as)
	}
	audit := get(h, "/api/v1/audit", c).Body.String()
	if !strings.Contains(audit, `"action":"asked node1 to act","detail":"reload admin.service, as node1:8443 · admin serves admin.internal (`) {
		t.Errorf("audit: %s", audit)
	}
}

// Step 3: a cert-manager Certificate close to its end is offered "Renew
// now" only when its cluster agent runs with -cert-renew and cert-manager
// isn't renewing it; the action goes and comes back with the agent's
// batches.
func TestCertRenewOffer(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	tok := NewAgentToken()
	srv, err := New(&Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []AgentConfig{{Name: "prod", TokenSHA256: HashToken(tok), Cluster: true}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	h := srv.Handler()
	c := devCookie(t, h)
	now := time.Now()
	ends := now.Add(5 * 24 * time.Hour).UTC().Format(time.RFC3339)
	cert := func(facts map[string]any) report.Workload {
		f := map[string]any{"names": "shop.example.org", "ends": ends, "certificate": "shop-tls", "issued_by": "ClusterIssuer letsencrypt"}
		for k, v := range facts {
			f[k] = v
		}
		return report.Workload{Kind: report.KindCert, ID: "c1", Name: "shop.example.org", Namespace: "shop", State: report.StateDown, Word: "Ends in 5 days", Since: now.Add(-85 * 24 * time.Hour), Facts: f}
	}
	cluster := func(renew bool) report.Workload {
		r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "prod", State: report.StateOK, Word: "API answering", Facts: map[string]any{"version": "v1.32.2"}}
		if renew {
			r.Facts["cert_renew"] = true
		}
		return r
	}
	gen := 0
	batch := func(b report.Workloads) *httptest.ResponseRecorder {
		gen++
		b.V, b.Agent, b.Generation = report.Version, "test", fmt.Sprint(gen)
		if b.Records == nil {
			b.Records = []report.Workload{}
		}
		body, _ := json.Marshal(b)
		rec := post(h, "/api/v1/agent/workloads", tok, body, nil)
		if rec.Code != 204 {
			t.Fatalf("batch: %d %s", rec.Code, rec.Body)
		}
		return rec
	}
	list := func(recs ...report.Workload) {
		batch(report.Workloads{Full: report.FullBegin})
		batch(report.Workloads{Full: report.FullEnd, Records: recs})
	}
	offers := func() []CertRenew {
		var p struct {
			Certificates []CertInv `json:"certificates"`
		}
		_ = json.Unmarshal(get(h, "/api/v1/certificates", c).Body.Bytes(), &p)
		if len(p.Certificates) != 1 {
			t.Fatalf("%d certificates", len(p.Certificates))
		}
		return p.Certificates[0].Renews
	}
	ask := `{"source":"prod","namespace":"shop","certificate":"shop-tls"}`

	list(cluster(false), cert(nil))
	if o := offers(); len(o) != 0 {
		t.Fatalf("offered without -cert-renew: %+v", o)
	}
	list(cluster(true), cert(map[string]any{"issuing": true}))
	if o := offers(); len(o) != 0 {
		t.Fatalf("offered while cert-manager renews: %+v", o)
	}
	list(cluster(true), cert(map[string]any{"ends": now.Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)}))
	if o := offers(); len(o) != 0 {
		t.Fatalf("offered with 60 days left: %+v", o)
	}
	if rec := sendCookie(h, "POST", "/api/v1/certificates/renew", c, ask); rec.Code != 409 {
		t.Fatalf("not on offer: %d %s", rec.Code, rec.Body)
	}

	list(cluster(true), cert(nil))
	o := offers()
	if len(o) != 1 || o[0].Source != "prod" || o[0].Namespace != "shop" || o[0].Certificate != "shop-tls" || !strings.HasSuffix(o[0].Where, " · shop · Certificate shop-tls") {
		t.Fatalf("offer: %+v", o)
	}
	if rec := sendCookie(h, "POST", "/api/v1/certificates/renew", c, ask); rec.Code != 200 {
		t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/certificates/renew", c, ask); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already waiting") {
		t.Fatalf("twice: %d %s", rec.Code, rec.Body)
	}
	// The next batch's answer says one waits; the agent takes it.
	if rec := batch(report.Workloads{}); rec.Header().Get("X-Beacon-Actions") != "1" {
		t.Fatal("no X-Beacon-Actions on the batch's answer")
	}
	req := httptest.NewRequest("GET", "/api/v1/agent/actions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var acts struct {
		Actions []report.Action `json:"actions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &acts)
	if len(acts.Actions) != 1 || acts.Actions[0].Kind != "renew" || string(acts.Actions[0].Args) != `{"namespace":"shop","name":"shop-tls"}` {
		t.Fatalf("actions: %s", rec.Body)
	}
	if rec := batch(report.Workloads{Actions: []report.ActionResult{{ID: acts.Actions[0].ID, OK: true, Detail: "asked cert-manager to renew Certificate shop/shop-tls"}}}); rec.Header().Get("X-Beacon-Actions") != "" {
		t.Fatal("still says actions wait")
	}
	if o := offers(); len(o) != 1 || o[0].Last == nil || o[0].Last.State != "done" {
		t.Fatalf("after: %+v", o)
	}
	audit := get(h, "/api/v1/audit", c).Body.String()
	for _, w := range []string{`"action":"asked prod to act","detail":"renew Certificate shop/shop-tls (shop.example.org, ends in 4 days`, `"action":"did action ` + acts.Actions[0].ID + `","detail":"asked cert-manager to renew Certificate shop/shop-tls"`} {
		if !strings.Contains(audit, w) {
			t.Errorf("audit lacks %s", w)
		}
	}
}
