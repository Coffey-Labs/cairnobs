package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
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

// fakeAPI answers as Proxmox VE's API does (pve-docs api-viewer), from
// fixed JSON, and counts what it was asked.
type fakeAPI struct {
	mu      sync.Mutex
	asked   map[string]int
	auth    string
	status  string
	configs map[string]string
}

const resources = `[
 {"id":"node/pve1","type":"node","node":"pve1","status":"online","maxcpu":32,"cpu":0.25,"maxmem":274877906944,"mem":68719476736,"uptime":86400},
 {"id":"node/pve2","type":"node","node":"pve2","status":"online","maxcpu":32,"cpu":"0.5","maxmem":274877906944,"mem":137438953472},
 {"id":"node/pve3","type":"node","node":"pve3","status":"offline","maxcpu":32,"maxmem":274877906944},
 {"id":"qemu/100","type":"qemu","vmid":100,"name":"db","node":"pve1","status":"running","maxcpu":4,"cpu":0.5,"maxmem":8589934592,"mem":4294967296,"hastate":"started","tags":"prod;db","pool":"shop","template":0},
 {"id":"qemu/101","type":"qemu","vmid":101,"name":"db","node":"pve2","status":"running","maxcpu":2,"cpu":0.1,"maxmem":4294967296,"mem":1073741824,"lock":"migrate"},
 {"id":"qemu/102","type":"qemu","vmid":102,"name":"build","node":"pve3","status":"stopped","maxcpu":8,"maxmem":17179869184,"hastate":"fence"},
 {"id":"qemu/103","type":"qemu","vmid":103,"name":"spare","node":"pve2","status":"stopped","maxcpu":2,"maxmem":2147483648},
 {"id":"qemu/9000","type":"qemu","vmid":9000,"name":"rhel9-template","node":"pve1","status":"stopped","template":1},
 {"id":"lxc/200","type":"lxc","vmid":200,"name":"proxy","node":"pve1","status":"running","maxcpu":1,"cpu":0.02,"maxmem":536870912,"mem":134217728},
 {"id":"storage/pve1/local","type":"storage","node":"pve1","storage":"local","status":"available","disk":1,"maxdisk":2}
]`

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked[r.Method+" "+r.URL.Path]++
	f.auth = r.Header.Get("Authorization")
	f.mu.Unlock()
	if r.Method != http.MethodGet {
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	switch {
	case p == "/cluster/resources":
		_, _ = w.Write([]byte(`{"data":` + resources + `}`))
	case p == "/cluster/status":
		_, _ = w.Write([]byte(`{"data":` + f.status + `}`))
	case p == "/version":
		_, _ = w.Write([]byte(`{"data":{"version":"8.4.1","release":"8.4","repoid":"2a5fa54a8503f96d"}}`))
	case strings.HasSuffix(p, "/config"):
		if c, ok := f.configs[p]; ok {
			_, _ = w.Write([]byte(`{"data":` + c + `}`))
			return
		}
		http.Error(w, "Permission check failed (/vms/103, VM.Audit)", http.StatusForbidden)
	default:
		http.NotFound(w, r)
	}
}

const quorate = `[{"type":"cluster","name":"prod-pve","quorate":1,"nodes":3,"version":7,"id":"cluster"},
 {"type":"node","name":"pve1","online":1,"local":1,"nodeid":1},{"type":"node","name":"pve2","online":1,"nodeid":2},{"type":"node","name":"pve3","online":0,"nodeid":3}]`

func setup(t *testing.T, pin bool) (*fakeAPI, *Client) {
	f := &fakeAPI{asked: map[string]int{}, status: quorate, configs: map[string]string{
		"/nodes/pve1/qemu/100/config": `{"smbios1":"uuid=5D307CA9-B3EF-428C-8861-06E72D69F223,manufacturer=QEMU","cores":4}`,
		"/nodes/pve2/qemu/101/config": `{"cores":2}`,
		"/nodes/pve3/qemu/102/config": `{"smbios1":"uuid=0b6a2b2e-1111-4222-8333-444455556666"}`,
	}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("obs@pve!beacon=8a4e0f5e-0000-4000-8000-000000000000\n"), 0o600)
	cfg := Config{URL: srv.URL, TokenFile: tok}
	if pin {
		sum := sha256.Sum256(srv.Certificate().Raw)
		cfg.Fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
	} else {
		ca := filepath.Join(dir, "pve-root-ca.pem")
		_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
		cfg.CAFile = ca
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestRead(t *testing.T) {
	f, c := setup(t, false)
	rd := NewReader(c)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	s, err := rd.Read(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if f.auth != "PVEAPIToken=obs@pve!beacon=8a4e0f5e-0000-4000-8000-000000000000" {
		t.Errorf("authorization %q", f.auth)
	}
	if s.Cluster != "prod-pve" || !s.Quorate || s.Nodes != 3 || s.Online != 2 || s.Version != "8.4.1" {
		t.Errorf("snapshot %+v", s)
	}
	if s.UUIDs[100] != "5d307ca9-b3ef-428c-8861-06e72d69f223" || s.UUIDs[101] != "" || s.UUIDs[102] == "" {
		t.Errorf("uuids %v", s.UUIDs)
	}
	if f.asked["GET /api2/json/nodes/pve1/qemu/9000/config"] != 0 {
		t.Error("read a template's config")
	}

	recs, use := Records(s)
	by := map[string]report.Workload{}
	for _, r := range recs {
		by[r.ID] = r
	}
	cl := by[report.ClusterSelf]
	// 103's config was refused: noted, not fatal.
	if cl.Name != "prod-pve" || cl.State != report.StateDegraded || !strings.Contains(cl.Word, "VM configs") || cl.Facts["version"] != "8.4.1" {
		t.Errorf("cluster %+v", cl)
	}
	for id, want := range map[string][2]string{
		"node/pve1": {report.StateOK, "pve1"}, "node/pve3": {report.StateDown, "pve3"},
		"qemu/100": {report.StateOK, "100 (db)"}, "qemu/101": {report.StateUpdating, "101 (db)"},
		"qemu/102": {report.StateDown, "102 (build)"}, "qemu/103": {report.StateStopped, "103 (spare)"},
		"lxc/200": {report.StateOK, "200 (proxy)"},
	} {
		if r := by[id]; r.State != want[0] || r.Name != want[1] {
			t.Errorf("%s: %s %q, want %v", id, r.State, r.Name, want)
		}
	}
	if _, ok := by["qemu/9000"]; ok || len(recs) != 1+3+5 {
		t.Errorf("%d records; a template among them: %v", len(recs), ok)
	}
	db := by["qemu/100"]
	if db.Node != "pve1" || db.Kind != report.KindVM || db.Facts["firmware_uuid"] != "5d307ca9-b3ef-428c-8861-06e72d69f223" ||
		db.Facts["tags"] != "prod, db" || db.Facts["pool"] != "shop" || db.Facts["type"] != "VM" || by["lxc/200"].Facts["type"] != "container" {
		t.Errorf("db: %+v", db)
	}
	u := map[string]report.Usage{}
	for _, x := range use {
		u[x.ID] = x
	}
	if len(u) != 5 || u["qemu/100"] != (report.Usage{ID: "qemu/100", CPU: 2, CPUOf: 4, Mem: 4 << 30, MemOf: 8 << 30}) ||
		u["node/pve2"].CPU != 16 || u["lxc/200"].CPU != 0.02 {
		t.Errorf("usage %+v", u)
	}

	// The next read keeps the UUIDs it has; a refused config is asked again.
	before := f.asked["GET /api2/json/nodes/pve1/qemu/100/config"]
	if _, err := rd.Read(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if f.asked["GET /api2/json/nodes/pve1/qemu/100/config"] != before || f.asked["GET /api2/json/version"] != 1 {
		t.Errorf("read again: %v", f.asked)
	}

	// Quorum lost: the cluster is down.
	f.status = `[{"type":"cluster","name":"prod-pve","quorate":0,"nodes":3},{"type":"node","name":"pve1","online":1}]`
	s, _ = rd.Read(context.Background(), now.Add(time.Minute))
	recs, _ = Records(s)
	if recs[0].State != report.StateDown || recs[0].Word != "Not quorate: 1 of 3 nodes online" {
		t.Errorf("not quorate: %+v", recs[0])
	}

	// A node on its own has no cluster entry: it's its own cluster.
	f.status = `[{"type":"node","name":"pve1","online":1,"local":1}]`
	s, _ = rd.Read(context.Background(), now.Add(2*time.Minute))
	if s.Cluster != "pve1" || !s.Quorate || s.Clustered {
		t.Errorf("standalone: %+v", s)
	}
	for k := range f.asked {
		if !strings.HasPrefix(k, "GET ") {
			t.Errorf("asked %s", k)
		}
	}
}

func TestPinnedCertificate(t *testing.T) {
	_, c := setup(t, true)
	if _, err := NewReader(c).Read(context.Background(), time.Now()); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	// A pin that isn't this certificate's: refused.
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("u@pve!t=s"), 0o600)
	wrong, err := New(Config{URL: strings.TrimSuffix(c.base, "/api2/json"), TokenFile: tok, Fingerprint: strings.Repeat("ab:", 31) + "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(wrong).Read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("another certificate: %v", err)
	}
}

func TestBadConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("just-a-secret"), 0o600)
	if _, err := New(Config{TokenFile: tok, Fingerprint: "ab"}); err == nil {
		t.Error("a token without its user and id")
	}
	_ = os.WriteFile(tok, []byte("u@pve!t=s"), 0o600)
	if _, err := New(Config{TokenFile: tok, Fingerprint: "abcd"}); err == nil {
		t.Error("a short fingerprint")
	}
	if _, err := New(Config{TokenFile: tok, CAFile: filepath.Join(dir, "none.pem")}); err == nil {
		t.Error("a missing CA")
	}
}
