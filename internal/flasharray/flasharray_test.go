package flasharray

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

	"github.com/cairnobs/cairnobs/internal/purerest"
	"github.com/cairnobs/cairnobs/internal/report"
)

// fakeFA is a fake of Purity's documented REST 2: api_version, login with
// an API token for a session, and the lists read, paged with
// continuation tokens.
type fakeFA struct {
	mu       sync.Mutex
	token    string
	sessions map[string]bool
	lists    map[string][]any
	fail     map[string]int
	calls    []string
	queries  map[string]string
	logins   int
}

func (f *fakeFA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/api/api_version" {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": []string{"1.17", "1.19", "2.0", "2.4", "2.36"}})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api/2.36")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if path == "/login" && r.Method == http.MethodPost {
		if r.Header.Get("api-token") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Invalid API token."}]}`))
			return
		}
		f.logins++
		s := fmt.Sprintf("session-%d", f.logins)
		f.sessions[s] = true
		w.Header().Set("x-auth-token", s)
		_, _ = w.Write([]byte(`{"items":[{"username":"obs"}]}`))
		return
	}
	if !f.sessions[r.Header.Get("x-auth-token")] {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Session expired."}]}`))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "read only", http.StatusMethodNotAllowed)
		return
	}
	if code := f.fail[path]; code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Permission denied."}]}`))
		return
	}
	all, ok := f.lists[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if v := q.Get("filter") + q.Get("destroyed"); v != "" {
		f.queries[path] = v
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	from, _ := strconv.Atoi(q.Get("continuation_token"))
	to := min(from+limit, len(all))
	out := map[string]any{"items": all[from:to], "total_item_count": len(all)}
	if to < len(all) {
		out["continuation_token"] = strconv.Itoa(to)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeFA) set(path string, v any) {
	b, _ := json.Marshal(v)
	var l []any
	_ = json.Unmarshal(b, &l)
	f.lists[path] = l
}

func volID(n int) string { return fmt.Sprintf("6b1a3c2d-0000-4000-8000-%012d", n) }

// array is fa-prod-01: two controllers, a failed drive, 1,200 volumes (three
// pages), an ActiveCluster pod with its peer resyncing, two connections
// and four open alerts.
func array() *fakeFA {
	f := &fakeFA{token: "t-obs-readonly", sessions: map[string]bool{}, lists: map[string][]any{}, fail: map[string]int{}, queries: map[string]string{}}
	parity := 1.0
	f.set("/arrays", []map[string]any{{"id": "a-1", "name": "fa-prod-01", "os": "Purity//FA", "version": "6.7.3", "capacity": int64(500) << 40,
		"parity": parity, "space": map[string]any{"total_physical": int64(410) << 40, "data_reduction": 4.23}}})
	f.set("/controllers", []map[string]any{
		{"name": "CT0", "mode": "primary", "status": "ready", "type": "array_controller", "model": "FA-X70R4", "version": "6.7.3"},
		{"name": "CT1", "mode": "secondary", "status": "ready", "type": "array_controller", "model": "FA-X70R4", "version": "6.7.3"},
		{"name": "SH9.SC0", "mode": "not present", "status": "ready", "type": "shelf_controller"},
	})
	f.set("/hardware", []map[string]any{{"name": "CT0.FAN0", "status": "healthy", "type": "fan"}, {"name": "CH0.PWR1", "status": "healthy", "type": "pwr"}})
	f.set("/drives", []map[string]any{{"name": "CH0.BAY3", "status": "failed", "details": "Drive reported a media error", "type": "SSD"},
		{"name": "CH0.BAY4", "status": "healthy", "type": "SSD"}, {"name": "CH0.BAY20", "status": "empty"}})
	var vols []map[string]any
	for n := range 1200 {
		v := map[string]any{"id": volID(n), "name": fmt.Sprintf("k8s-pvc-%04d", n), "serial": fmt.Sprintf("ABCDEF%018d", n), "provisioned": int64(100) << 30,
			"connection_count": 1, "created": time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), "subtype": "regular",
			"space": map[string]any{"unique": int64(10) << 30, "snapshots": int64(2) << 30, "data_reduction": 3.0}}
		switch n {
		case 5:
			v["connection_count"] = 0
		case 6:
			v["pod"] = map[string]any{"name": "prod-pod"}
		case 7:
			v["subtype"] = "protocol_endpoint"
		}
		vols = append(vols, v)
	}
	f.set("/volumes", vols)
	f.set("/pods", []map[string]any{
		{"id": "p-1", "name": "prod-pod", "arrays": []map[string]any{{"name": "fa-prod-01", "status": "online", "mediator_status": "online"},
			{"name": "fa-prod-02", "status": "resyncing", "progress": 0.43, "mediator_status": "online"}}},
		{"id": "p-2", "name": "local-pod", "arrays": []map[string]any{{"name": "fa-prod-01", "status": "online", "mediator_status": "unreachable"}}},
	})
	f.set("/array-connections", []map[string]any{
		{"id": "c-1", "name": "fa-prod-02", "status": "connected", "type": "sync-replication", "version": "6.7.3"},
		{"id": "c-2", "name": "fa-dr-01", "status": "connecting", "type": "async-replication"},
	})
	f.set("/arrays/performance", []map[string]any{{"id": "a-1", "name": "fa-prod-01", "reads_per_sec": 41200, "writes_per_sec": 18300,
		"usec_per_read_op": 310, "usec_per_write_op": 145, "read_bytes_per_sec": 2.1e9, "write_bytes_per_sec": 0.9e9, "time": 1794600000000}})
	f.set("/alerts", []map[string]any{
		{"id": "al-1", "code": 32, "severity": "critical", "state": "open", "summary": "Drive failure", "component_name": "CH0.BAY3", "component_type": "drive"},
		{"id": "al-2", "code": 7, "severity": "warning", "state": "open", "summary": "Interface down", "component_name": "ct1.eth4", "component_type": "eth_port"},
		{"id": "al-3", "code": 41, "severity": "critical", "state": "open", "summary": "Volume space exceeded", "component_name": "k8s-pvc-0009", "component_type": "volume"},
		{"id": "al-4", "code": 1, "severity": "hidden", "state": "open", "summary": "Internal", "component_name": "x"},
	})
	return f
}

func setup(t *testing.T, f *fakeFA, pin bool) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte(f.token+"\n"), 0o600)
	cfg := Config{URL: srv.URL, TokenFile: filepath.Join(dir, "token")}
	if pin {
		sum := sha256.Sum256(srv.Certificate().Raw)
		cfg.Fingerprint = hex.EncodeToString(sum[:])
	} else {
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
	f := array()
	c := setup(t, f, false)
	if c.Name() != "127.0.0.1" {
		t.Errorf("name %q", c.Name())
	}
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Version() != "2.36" || len(s.Volumes) != 1200 || len(s.Controllers) != 3 || len(s.Unhealthy) != 1 || len(s.Pods) != 2 ||
		len(s.Connections) != 2 || len(s.Alerts) != 4 || len(s.Problems) != 0 {
		t.Fatalf("version %s, %d volumes, %d controllers, %d unhealthy, %d pods, %d connections, %d alerts, problems %v",
			c.Version(), len(s.Volumes), len(s.Controllers), len(s.Unhealthy), len(s.Pods), len(s.Connections), len(s.Alerts), s.Problems)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && call != "POST /api/2.36/login" {
			t.Errorf("asked %s", call)
		}
	}
	if f.logins != 1 || f.queries["/alerts"] != "state='open'" || f.queries["/volumes"] != "false" || f.queries["/pods"] != "false" {
		t.Errorf("%d logins, queries %v", f.logins, f.queries)
	}
	recs := Records(s)
	if len(recs) != 1+2+1199+2+2 {
		t.Fatalf("%d records", len(recs))
	}
	m := byID(recs)
	self := recs[0]
	if self.ID != report.ClusterSelf || self.Name != "fa-prod-01" || self.Facts["version"] != "6.7.3" || self.Facts["distribution"] != "Purity//FA" ||
		self.Facts["used_pct"] != 82.0 || self.Facts["model"] != "FA-X70R4" || self.State != report.StateDegraded ||
		self.Word != "Hardware: CH0.BAY3 failed (Drive reported a media error)" || self.Facts["alerts"] != "CH0.BAY3: Drive failure (critical)" {
		t.Errorf("the array %+v", self)
	}
	want := map[string][2]string{
		"ct0":            {report.StateOK, "Ready, primary"},
		"ct1":            {report.StateOK, "Ready, secondary"}, // a warning only
		volID(0):         {report.StateOK, "Connected"},
		volID(5):         {report.StateOK, "Not connected to a host"},
		volID(9):         {report.StateDegraded, "Alert from Purity: Volume space exceeded"},
		"pod/p-1":        {report.StateUpdating, "Resyncing on fa-prod-02 (43%)"},
		"pod/p-2":        {report.StateDegraded, "Mediator unreachable from fa-prod-01"},
		"connection/c-1": {report.StateOK, "Connected"},
		"connection/c-2": {report.StateDown, "Not connected (trying)"},
	}
	for id, sw := range want {
		if r := m[id]; r.State != sw[0] || r.Word != sw[1] {
			t.Errorf("%s: %s %q, want %s %q", id, r.State, r.Word, sw[0], sw[1])
		}
	}
	if _, ok := m[volID(7)]; ok {
		t.Error("a protocol endpoint sent")
	}
	if _, ok := m["sh9.sc0"]; ok {
		t.Error("a shelf controller sent as a node")
	}
	if m["ct1"].Facts["alerts"] != "ct1.eth4: Interface down (warning)" {
		t.Errorf("ct1 %+v", m["ct1"])
	}
	if v := m[volID(6)]; v.Kind != report.KindPVC || v.Name != "k8s-pvc-0006" || v.Facts["pod"] != "prod-pod" || v.Facts["provisioned_gib"] != 100.0 ||
		v.Facts["used_gib"] != 12.0 || v.Facts["serial"] != fmt.Sprintf("abcdef%018d", 6) {
		t.Errorf("volume %+v", v)
	}
	if p := m["pod/p-1"]; p.Kind != report.KindCluster || p.Name != "pod/prod-pod" {
		t.Errorf("pod %+v", p)
	}
	us := Usage(s)
	if len(us) != 1 || us[0].ID != report.ClusterSelf || us[0].IO == nil {
		t.Fatalf("usage %+v", us)
	}
	if io := *us[0].IO; io.ReadOps != 41200 || io.WriteOps != 18300 || io.ReadUs != 310 || io.WriteUs != 145 || io.ReadBps != 2.1e9 ||
		io.WriteBps != 0.9e9 || io.Used != s.Array.Space.TotalPhysical || io.UsedOf != int64(500)<<40 {
		t.Errorf("io %+v", io)
	}
	if _, ok := self.Facts["performance"]; ok {
		t.Errorf("performance %v", self.Facts["performance"])
	}
}

// Performance is for history: an array that won't give it is still fine.
func TestNoPerformance(t *testing.T) {
	f := array()
	f.fail["/arrays/performance"] = http.StatusForbidden
	c := setup(t, f, false)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	self := Records(s)[0]
	if Usage(s) != nil || len(s.Problems) != 0 || !strings.HasPrefix(fmt.Sprint(self.Facts["performance"]), "can't read it: flasharray: 403") {
		t.Errorf("usage %v, problems %v, fact %v", Usage(s), s.Problems, self.Facts["performance"])
	}
}

func TestSessionLapses(t *testing.T) {
	f := array()
	c := setup(t, f, true)
	if _, err := c.Read(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sessions = map[string]bool{} // the array forgot it
	f.mu.Unlock()
	if _, err := c.Read(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("%d logins", f.logins)
	}
}

func TestPartlyReadable(t *testing.T) {
	f := array()
	f.fail["/pods"] = http.StatusForbidden
	c := setup(t, f, false)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	self := Records(s)[0]
	if !strings.Contains(self.Word, "can't read pods: flasharray: 403 Permission denied.") {
		t.Errorf("the array %q", self.Word)
	}
	f.fail["/arrays"] = http.StatusForbidden
	if _, err := c.Read(context.Background(), time.Now()); err == nil {
		t.Error("read without the array")
	}
}

func TestRefused(t *testing.T) {
	f := array()
	c := setup(t, f, false)
	f.token = "another" // the user's token was replaced
	_, err := c.Read(context.Background(), time.Now())
	var se *purerest.StatusError
	if !errors.As(err, &se) || se.Code != 401 || se.Msg != "logging in: Invalid API token." {
		t.Errorf("%v", err)
	}
	// An array without REST 2.
	old := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":["1.16","1.17"]}`))
	}))
	defer old.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte("t"), 0o600)
	sum := sha256.Sum256(old.Certificate().Raw)
	c, _ = New(Config{URL: old.URL, TokenFile: filepath.Join(dir, "token"), Fingerprint: hex.EncodeToString(sum[:])})
	if _, err := c.Read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "Purity 6.0 or later") {
		t.Errorf("REST 1 only: %v", err)
	}
	// A wrong pin.
	c, _ = New(Config{URL: old.URL, TokenFile: filepath.Join(dir, "token"), Fingerprint: strings.Repeat("ab", 32)})
	if _, err := c.Read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "isn't the pinned one") {
		t.Errorf("a wrong pin: %v", err)
	}
}

func TestBadConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("t"), 0o600)
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	for _, cfg := range []Config{
		{URL: "http://fa", TokenFile: tok},
		{URL: "https://fa"},
		{URL: "https://fa", TokenFile: empty},
		{URL: "https://fa", TokenFile: tok, Fingerprint: "abc"},
		{URL: "https://fa", TokenFile: tok, CAFile: filepath.Join(dir, "none.pem")},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	c := Config{URL: "https://FA-Prod-01.example.org"}
	c.Defaults()
	if c.Name != "fa-prod-01.example.org" || c.Every != time.Minute {
		t.Errorf("%+v", c)
	}
}

func TestUnreachable(t *testing.T) {
	c := setup(t, array(), false)
	s, _ := c.Read(context.Background(), time.Now())
	last := Records(s)
	down := Unreachable("fa", last, errors.New("dial tcp: i/o timeout"))
	if len(down) != len(last) || down[0].State != report.StateDown || down[0].Name != "fa-prod-01" || down[0].Word != "Can't reach its API: dial tcp: i/o timeout" {
		t.Errorf("%+v", down[0])
	}
}
