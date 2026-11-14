package nutanix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// fakePC is a fake of Prism Central's documented v4 API: the five lists
// read, paged as v4 pages them, behind basic auth or an API key.
type fakePC struct {
	mu      sync.Mutex
	lists   map[string][]any
	fail    map[string]int // path → status to answer with
	key     string
	calls   []string
	filters []string
}

func (f *fakePC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet {
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	u, p, ok := r.BasicAuth()
	if !(ok && u == "obs" && p == "s3cret") && (f.key == "" || r.Header.Get("X-Ntnx-Api-Key") != f.key) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"data":{"error":[{"message":"Authentication required."}]}}`))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")
	if code := f.fail[path]; code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"data":{"error":[{"message":"Access denied for this entity."}]}}`))
		return
	}
	all, ok := f.lists[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if fl := r.URL.Query().Get("$filter"); fl != "" {
		f.filters = append(f.filters, fl)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("$page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("$limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	lo, hi := min(page*limit, len(all)), min((page+1)*limit, len(all))
	_ = json.NewEncoder(w).Encode(map[string]any{"data": all[lo:hi], "metadata": map[string]any{"totalAvailableResults": len(all)}})
}

func (f *fakePC) set(path string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(v)
	var l []any
	_ = json.Unmarshal(b, &l)
	f.lists[path] = l
}

const (
	pcID  = "00061e0b-pc00-0000-0000-000000000001"
	pe1ID = "00061e0b-pe01-0000-0000-000000000001"
	pe2ID = "00061e0b-pe02-0000-0000-000000000001"
)

func hostID(n int) string { return fmt.Sprintf("a1b2c3d4-0000-4000-8000-%012d", n) }
func vmID(n int) string   { return fmt.Sprintf("5f0e1d2c-0000-4000-8000-%012d", n) }

// prism is two clusters of three AHV hosts and Prism Central, 150 VMs
// (two pages), three containers (one Nutanix's own) and four open alerts.
func prism() *fakePC {
	f := &fakePC{lists: map[string][]any{}, fail: map[string]int{}}
	yes, no := true, false
	clusters := []map[string]any{
		{"extId": pcID, "name": "pc-01", "config": map[string]any{"clusterFunction": []string{"PRISM_CENTRAL"}, "buildInfo": map[string]any{"version": "pc.2024.3.1"}}},
		{"extId": pe1ID, "name": "pe-prod-01", "config": map[string]any{"clusterFunction": []string{"AOS"}, "isAvailable": yes, "buildInfo": map[string]any{"version": "6.10"}}},
		{"extId": pe2ID, "name": "pe-prod-02", "config": map[string]any{"clusterFunction": []string{"AOS"}, "isAvailable": no}},
	}
	f.set(clustersPath, clusters)
	var hosts []map[string]any
	for n := 1; n <= 6; n++ {
		cid, cname := pe1ID, "pe-prod-01"
		if n > 3 {
			cid, cname = pe2ID, "pe-prod-02"
		}
		h := map[string]any{"extId": hostID(n), "hostName": fmt.Sprintf("ahv-%02d", n), "hostType": "HYPER_CONVERGED",
			"cluster": map[string]any{"uuid": cid, "name": cname}, "blockModel": "NX-3170-G8", "numberOfCpuCores": 32,
			"memorySizeBytes": int64(512) << 30, "nodeStatus": "NORMAL",
			"hypervisor": map[string]any{"fullName": "AHV 10.0.1", "type": "AHV", "state": "ACROPOLIS_NORMAL", "acropolisConnectionState": "CONNECTED"}}
		switch n {
		case 2:
			h["hypervisor"].(map[string]any)["state"] = "ENTERED_MAINTENANCE_MODE"
		case 5:
			h["hypervisor"].(map[string]any)["acropolisConnectionState"] = "DISCONNECTED"
		case 6:
			h["isDegraded"] = true
		}
		hosts = append(hosts, h)
	}
	f.set(hostsPath, hosts)
	var vms []map[string]any
	for n := range 150 {
		v := map[string]any{"extId": vmID(n), "name": fmt.Sprintf("app-%03d", n), "biosUuid": strings.ToUpper(vmID(1000 + n)),
			"powerState": "ON", "cluster": map[string]any{"extId": pe1ID}, "host": map[string]any{"extId": hostID(n%3 + 1)},
			"numSockets": 2, "numCoresPerSocket": 2, "memorySizeBytes": int64(8) << 30,
			"guestTools": map[string]any{"isInstalled": true, "isReachable": true, "guestOsVersion": "Rocky Linux 9.5"}}
		switch n {
		case 7:
			v["powerState"] = "OFF"
			delete(v, "host")
		case 8:
			v["powerState"] = "PAUSED"
		case 9:
			v["guestTools"].(map[string]any)["isReachable"] = false
		case 10, 11:
			v["name"] = "clone" // two share a name
		}
		vms = append(vms, v)
	}
	f.set(vmsPath, vms)
	f.set(containersPath, []map[string]any{
		{"extId": "c-1", "name": "default-container", "clusterExtId": pe1ID, "clusterName": "pe-prod-01", "maxCapacityBytes": int64(40) << 40, "replicationFactor": 2},
		{"extId": "c-2", "name": "NutanixManagementShare", "clusterExtId": pe1ID, "isInternal": true},
		{"extId": "c-3", "name": "old-images", "clusterExtId": pe2ID, "isMarkedForRemoval": true},
	})
	f.set(alertsPath, []map[string]any{
		{"extId": "al-1", "title": "Storage Container Space Usage Exceeded", "severity": "CRITICAL",
			"sourceEntity": map[string]any{"extId": "c-1", "type": "storage_container"}},
		{"extId": "al-2", "title": "VM Snapshot Is Old", "severity": "WARNING",
			"sourceEntity": map[string]any{"extId": vmID(3), "type": "vm"}},
		{"extId": "al-3", "title": "Disk Failure", "severity": "CRITICAL", "isAcknowledged": true,
			"sourceEntity": map[string]any{"extId": hostID(1), "type": "host"}, "affectedEntities": []map[string]any{{"extId": hostID(1)}}},
		{"extId": "al-4", "title": "NTP Not Synchronized", "severity": "WARNING",
			"sourceEntity": map[string]any{"extId": pe1ID, "type": "cluster"}},
	})
	return f
}

func setup(t *testing.T, f *fakePC, how string) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret\n"), 0o600)
	cfg := Config{URL: srv.URL, Username: "obs", PasswordFile: filepath.Join(dir, "password")}
	switch how {
	case "pin":
		sum := sha256.Sum256(srv.Certificate().Raw)
		cfg.Fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
	case "key":
		f.key = "k3y-for-obs"
		_ = os.WriteFile(filepath.Join(dir, "key"), []byte(f.key+"\n"), 0o600)
		cfg.Username, cfg.PasswordFile, cfg.APIKeyFile = "", "", filepath.Join(dir, "key")
		fallthrough
	default:
		ca := filepath.Join(dir, "ca.pem")
		_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
		cfg.CAFile = ca
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func byID(recs []report.Workload) map[string]report.Workload {
	m := map[string]report.Workload{}
	for _, r := range recs {
		m[r.ID] = r
	}
	return m
}

func TestRead(t *testing.T) {
	f := prism()
	c := setup(t, f, "ca")
	now := time.Now()
	s, err := c.Read(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.VMs) != 150 || len(s.Hosts) != 6 || len(s.Clusters) != 3 || len(s.Containers) != 3 || len(s.Alerts) != 4 || len(s.Problems) != 0 {
		t.Fatalf("%d VMs, %d hosts, %d clusters, %d containers, %d alerts, problems %v", len(s.VMs), len(s.Hosts), len(s.Clusters), len(s.Containers), len(s.Alerts), s.Problems)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("asked %s", call)
		}
	}
	if len(f.filters) != 1 || f.filters[0] != "isResolved eq false" {
		t.Errorf("filters %q", f.filters)
	}
	recs := Records(s)
	if len(recs) != 1+6+150+2 {
		t.Fatalf("%d records", len(recs))
	}
	m := byID(recs)
	self := recs[0]
	if self.ID != report.ClusterSelf || self.Name != "127.0.0.1" || self.Facts["distribution"] != "Prism Central" || self.Facts["version"] != "pc.2024.3.1" ||
		self.Facts["clusters"] != "pe-prod-01, pe-prod-02" || self.State != report.StateDegraded || self.Word != "Cluster pe-prod-02 not available" ||
		self.Facts["alerts"] != "pe-prod-01: NTP Not Synchronized (warning)" {
		t.Errorf("Prism Central %+v", self)
	}
	want := map[string][2]string{
		hostID(1): {report.StateOK, "Connected"}, // its critical alert is acknowledged
		hostID(2): {report.StateUpdating, "In maintenance mode"},
		hostID(5): {report.StateDown, "Disconnected from its cluster"},
		hostID(6): {report.StateDegraded, "Marked degraded by its cluster"},
		vmID(0):   {report.StateOK, "Running"},
		vmID(3):   {report.StateOK, "Running"}, // a warning only
		vmID(7):   {report.StateStopped, "Powered off"},
		vmID(8):   {report.StateStopped, "Paused"},
		"c-1":     {report.StateDegraded, "Alert from Prism: Storage Container Space Usage Exceeded"},
		"c-3":     {report.StateUpdating, "Marked for removal"},
	}
	for id, sw := range want {
		if r := m[id]; r.State != sw[0] || r.Word != sw[1] {
			t.Errorf("%s: %s %q, want %s %q", id, r.State, r.Word, sw[0], sw[1])
		}
	}
	if _, ok := m["c-2"]; ok {
		t.Error("Nutanix's own container sent")
	}
	if v := m[vmID(0)]; v.Kind != report.KindVM || v.Node != "ahv-01" || v.Facts["vcpus"] != 4 || v.Facts["memory_gib"] != 8.0 ||
		v.Facts["firmware_uuid"] != vmID(1000) || v.Facts["cluster"] != "pe-prod-01" || v.Facts["guest_os"] != "Rocky Linux 9.5" || v.Facts["hypervisor"] != "AHV" {
		t.Errorf("VM %+v", v)
	}
	if m[vmID(7)].Node != "" || m[vmID(9)].Facts["guest_tools"] != "not reachable" || m[vmID(3)].Facts["alerts"] != "VM Snapshot Is Old (warning)" {
		t.Errorf("VMs 7, 9, 3: %+v %+v %+v", m[vmID(7)], m[vmID(9)], m[vmID(3)])
	}
	if m[vmID(10)].Name != "clone ("+vmID(10)+")" || m[vmID(11)].Name != "clone ("+vmID(11)+")" {
		t.Errorf("shared names %q %q", m[vmID(10)].Name, m[vmID(11)].Name)
	}
	if h := m[hostID(1)]; h.Kind != report.KindNode || h.Facts["cluster"] != "pe-prod-01" || h.Facts["version"] != "AHV 10.0.1" || h.Facts["alerts"] != "Disk Failure (critical)" {
		t.Errorf("host %+v", h)
	}
}

func TestPartlyReadable(t *testing.T) {
	f := prism()
	f.fail[alertsPath] = http.StatusForbidden
	c := setup(t, f, "pin")
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	self := Records(s)[0]
	if self.State != report.StateDegraded || !strings.Contains(self.Word, "can't read alerts: nutanix: 403 Access denied for this entity.") {
		t.Errorf("Prism Central %s %q", self.State, self.Word)
	}
	f.fail[vmsPath] = http.StatusForbidden
	if _, err := c.Read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "VMs: nutanix: 403") {
		t.Errorf("no VMs read: %v", err)
	}
}

func TestAPIKey(t *testing.T) {
	f := prism()
	c := setup(t, f, "key")
	if _, err := c.Read(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestWrongPassword(t *testing.T) {
	f := prism()
	c := setup(t, f, "ca")
	c.auth = func(r *http.Request) { r.SetBasicAuth("obs", "wrong") }
	_, err := c.Read(context.Background(), time.Now())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 || se.Msg != "Authentication required." {
		t.Errorf("%v", err)
	}
}

func TestPinnedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(prism())
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret"), 0o600)
	c, err := New(Config{URL: srv.URL, Username: "obs", PasswordFile: filepath.Join(dir, "password"), Fingerprint: strings.Repeat("ab:", 31) + "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "isn't the pinned one") {
		t.Errorf("a wrong pin: %v", err)
	}
	// No pin and no CA: the system's CAs, which don't know a test server.
	c, _ = New(Config{URL: srv.URL, Username: "obs", PasswordFile: filepath.Join(dir, "password")})
	if _, err := c.Read(context.Background(), time.Now()); err == nil {
		t.Error("an unknown CA trusted")
	}
}

func TestBadConfig(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "password")
	_ = os.WriteFile(pw, []byte("x"), 0o600)
	for _, cfg := range []Config{
		{URL: "http://pc:9440", Username: "obs", PasswordFile: pw},
		{URL: "https://pc:9440"},
		{URL: "https://pc:9440", Username: "obs", PasswordFile: pw, APIKeyFile: pw},
		{URL: "https://pc:9440", Username: "obs", PasswordFile: filepath.Join(dir, "none")},
		{URL: "https://pc:9440", Username: "obs", PasswordFile: pw, Fingerprint: "abcd"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	c, err := New(Config{URL: "https://pc.example.org", Username: "obs", PasswordFile: pw})
	if err != nil || c.base != "https://pc.example.org:9440/api" || c.Name() != "pc.example.org" {
		t.Errorf("%v %+v", err, c)
	}
}

func TestUnreachable(t *testing.T) {
	f := prism()
	c := setup(t, f, "ca")
	s, _ := c.Read(context.Background(), time.Now())
	last := Records(s)
	down := Unreachable("pc-01", last, errors.New("dial tcp: connection refused"))
	if len(down) != len(last) || down[0].State != report.StateDown || down[0].Word != "Can't reach its API: dial tcp: connection refused" || down[0].Name != "127.0.0.1" {
		t.Errorf("%d of %d, %+v", len(down), len(last), down[0])
	}
	if first := Unreachable("pc-01", nil, errors.New("timeout")); len(first) != 1 || first[0].Name != "pc-01" {
		t.Errorf("%+v", first)
	}
}
