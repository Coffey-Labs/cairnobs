package flashblade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// fakeFB is a fake of Purity//FB's documented REST 2: api_version, login
// at /api/login (not under the version, as a FlashArray's is) for a
// session, and the lists read, paged with continuation tokens.
type fakeFB struct {
	mu       sync.Mutex
	token    string
	sessions map[string]bool
	lists    map[string][]any
	fail     map[string]int
	calls    []string
	queries  map[string]string
	logins   int
}

func (f *fakeFB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch r.URL.Path {
	case "/api/api_version":
		_ = json.NewEncoder(w).Encode(map[string]any{"versions": nil, "version": []string{"1.12", "2.0", "2.9", "2.17"}})
		return
	case "/api/login":
		if r.Method != http.MethodPost || r.Header.Get("api-token") != f.token {
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
	case "/api/logout":
		delete(f.sessions, r.Header.Get("x-auth-token"))
		_, _ = w.Write([]byte(`{}`))
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api/2.17")
	if !ok || path == "/login" { // a FlashArray's login: not a FlashBlade's
		http.NotFound(w, r)
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

func (f *fakeFB) set(path string, v any) {
	b, _ := json.Marshal(v)
	var l []any
	_ = json.Unmarshal(b, &l)
	f.lists[path] = l
}

func fsID(n int) string { return fmt.Sprintf("1f5b0000-0000-4000-8000-%012d", n) }

// blade is fb-prod-01: ten blades (one evacuating, one critical) and two
// empty slots, a failed fan, 700 file systems (two pages), three buckets,
// replica links (one unhealthy), two connections and three open alerts.
func blade() *fakeFB {
	f := &fakeFB{token: "t-obs-readonly", sessions: map[string]bool{}, lists: map[string][]any{}, fail: map[string]int{}, queries: map[string]string{}}
	f.set("/arrays", []map[string]any{{"id": "a-1", "name": "fb-prod-01", "os": "Purity//FB", "version": "4.5.6", "product_type": "FlashBlade//S"}})
	f.set("/arrays/space", []map[string]any{{"capacity": int64(2000) << 40, "parity": 1.0,
		"space": map[string]any{"data_reduction": 3.14, "total_physical": int64(1300) << 40, "total_used": int64(1300) << 40}}})
	var blades []map[string]any
	for i := 1; i <= 12; i++ {
		b := map[string]any{"id": fmt.Sprintf("b-%d", i), "name": fmt.Sprintf("CH1.FB%d", i), "status": "healthy", "raw_capacity": int64(48) << 40}
		switch i {
		case 3:
			b["status"], b["progress"] = "evacuating", 0.37
		case 7:
			b["status"], b["details"] = "critical", "Blade is not responding"
		case 11, 12:
			b["status"], b["raw_capacity"] = "unused", 0
		}
		blades = append(blades, b)
	}
	f.set("/blades", blades)
	f.set("/hardware", []map[string]any{
		{"name": "CH1", "status": "healthy", "type": "ch"},
		{"name": "CH1.FB7", "status": "critical", "type": "fb"}, // a blade: its own record
		{"name": "CH1.FAN2", "status": "critical", "type": "fan", "details": "Fan speed is 0 RPM"},
	})
	var fss []map[string]any
	for i := range 700 {
		fs := map[string]any{"id": fsID(i), "name": fmt.Sprintf("px_8f3a2b1c-pvc-%08d-0000-4000-8000-000000000000", i), "created": int64(1790000000000),
			"provisioned": int64(1) << 40, "destroyed": false, "promotion_status": "promoted", "writable": true,
			"space": map[string]any{"total_physical": int64(200) << 30, "total_used": int64(200) << 30, "data_reduction": 2.5},
			"nfs":   map[string]any{"v3_enabled": true, "v4_1_enabled": true}, "smb": map[string]any{"enabled": false}, "http": map[string]any{"enabled": false}}
		switch i {
		case 0:
			fs["name"] = "home"
			fs["smb"] = map[string]any{"enabled": true}
		case 1:
			fs["name"], fs["promotion_status"] = "home-dr", "demoted"
		case 2:
			fs["name"] = "scratch"
			fs["nfs"] = map[string]any{"v3_enabled": false, "v4_1_enabled": false}
		}
		fss = append(fss, fs)
	}
	f.set("/file-systems", fss)
	f.set("/buckets", []map[string]any{
		{"id": "bk-1", "name": "backups", "account": map[string]any{"name": "veeam"}, "object_count": 1200000, "quota_limit": int64(100) << 40, "versioning": "enabled",
			"space": map[string]any{"total_physical": int64(40) << 40}},
		{"id": "bk-2", "name": "logs", "account": map[string]any{"name": "ops"}, "object_count": 52000, "versioning": "none", "space": map[string]any{"total_used": int64(2) << 40}},
		{"id": "bk-3", "name": "images", "account": map[string]any{"name": "web"}, "object_count": 9, "space": map[string]any{"total_used": 1 << 30}},
	})
	f.set("/file-system-replica-links", []map[string]any{
		{"id": "l-1", "direction": "outbound", "status": "replicating", "lag": 240000, "local_file_system": map[string]any{"name": "home"}, "remote": map[string]any{"name": "fb-dr-01"}},
		{"id": "l-2", "direction": "outbound", "status": "unhealthy", "status_details": "Target file system is full", "lag": int64(5 * 3600 * 1000),
			"local_file_system": map[string]any{"name": "scratch"}, "remote": map[string]any{"name": "fb-dr-01"}},
	})
	f.set("/bucket-replica-links", []map[string]any{
		{"id": "l-3", "direction": "outbound", "status": "paused", "paused": true, "local_bucket": map[string]any{"name": "backups"}, "remote": map[string]any{"name": "s3-offsite"}},
	})
	f.set("/array-connections", []map[string]any{
		{"id": "c-1", "remote": map[string]any{"name": "fb-dr-01"}, "status": "connected", "type": "async-replication", "version": "4.5.6"},
		{"id": "c-2", "remote": map[string]any{"name": "fb-lab-01"}, "status": "incompatible", "type": "async-replication", "version": "3.3.1"},
	})
	f.set("/alerts", []map[string]any{
		{"id": "al-1", "code": 300, "severity": "critical", "state": "open", "summary": "Blade failure", "component_name": "CH1.FB7", "component_type": "blade"},
		{"id": "al-2", "code": 41, "severity": "warning", "state": "open", "summary": "File system nearing its hard limit", "component_name": "logs", "component_type": "bucket"},
		{"id": "al-3", "code": 18, "severity": "critical", "state": "open", "summary": "Fan failure", "component_name": "CH1.FAN2", "component_type": "fan"},
	})
	f.set("/arrays/performance", []map[string]any{{"reads_per_sec": 120000, "writes_per_sec": 30000, "usec_per_read_op": 900, "usec_per_write_op": 1500,
		"read_bytes_per_sec": 9e9, "write_bytes_per_sec": 2e9}})
	return f
}

func setup(t *testing.T, f *fakeFB) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte(f.token+"\n"), 0o600)
	sum := sha256.Sum256(srv.Certificate().Raw)
	c, err := New(Config{URL: srv.URL, TokenFile: filepath.Join(dir, "token"), Fingerprint: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRead(t *testing.T) {
	f := blade()
	c := setup(t, f)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Version() != "2.17" || len(s.FileSystems) != 700 || len(s.Blades) != 12 || len(s.Unhealthy) != 1 || len(s.Buckets) != 3 ||
		len(s.Links) != 3 || len(s.Connections) != 2 || len(s.Alerts) != 3 || len(s.Problems) != 0 || s.Perf == nil || s.Space == nil {
		t.Fatalf("version %s, %d file systems, %d blades, %d unhealthy, %d buckets, %d links, %d connections, %d alerts, problems %v",
			c.Version(), len(s.FileSystems), len(s.Blades), len(s.Unhealthy), len(s.Buckets), len(s.Links), len(s.Connections), len(s.Alerts), s.Problems)
	}
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "GET ") && call != "POST /api/login" {
			t.Errorf("asked %s", call)
		}
	}
	if f.logins != 1 || f.queries["/alerts"] != "state='open'" || f.queries["/file-systems"] != "false" || f.queries["/buckets"] != "false" {
		t.Errorf("%d logins, queries %v", f.logins, f.queries)
	}
	recs := Records(s)
	if len(recs) != 1+10+700+3+2 {
		t.Fatalf("%d records", len(recs))
	}
	m := map[string]report.Workload{}
	for _, r := range recs {
		m[r.ID] = r
	}
	self := recs[0]
	if self.ID != report.ClusterSelf || self.Name != "fb-prod-01" || self.Facts["distribution"] != "Purity//FB" || self.Facts["version"] != "4.5.6" ||
		self.Facts["model"] != "FlashBlade//S" || self.Facts["used_pct"] != 65.0 || self.Facts["capacity_tib"] != 2000.0 || self.State != report.StateDegraded ||
		self.Word != "Hardware: CH1.FAN2 critical (Fan speed is 0 RPM)" || self.Facts["alerts"] != "CH1.FAN2: Fan failure (critical)" {
		t.Errorf("the array %+v", self)
	}
	want := map[string][2]string{
		"ch1.fb1":        {report.StateOK, "Healthy"},
		"ch1.fb3":        {report.StateUpdating, "Evacuating its data (37%)"},
		"ch1.fb7":        {report.StateDown, "Critical: Blade is not responding"},
		fsID(0):          {report.StateOK, "Shared over NFSv3, NFSv4.1, SMB"},
		fsID(1):          {report.StateOK, "Demoted: a replication target, read only"},
		fsID(2):          {report.StateDegraded, "Replication to fb-dr-01 is unhealthy: Target file system is full"},
		fsID(5):          {report.StateOK, "Shared over NFSv3, NFSv4.1"},
		"bk-1":           {report.StateOK, "Ready"},
		"bk-2":           {report.StateOK, "Ready"}, // a warning only
		"connection/c-1": {report.StateOK, "Connected"},
		"connection/c-2": {report.StateDown, "Incompatible: the two arrays' versions can't replicate to each other"},
	}
	for id, sw := range want {
		if r := m[id]; r.State != sw[0] || r.Word != sw[1] {
			t.Errorf("%s: %s %q, want %s %q", id, r.State, r.Word, sw[0], sw[1])
		}
	}
	if _, ok := m["ch1.fb11"]; ok {
		t.Error("an empty slot sent as a blade")
	}
	if m["ch1.fb7"].Facts["alerts"] != "Blade failure (critical)" || m["bk-2"].Facts["alerts"] != "File system nearing its hard limit (warning)" {
		t.Errorf("alerts: %v, %v", m["ch1.fb7"].Facts, m["bk-2"].Facts)
	}
	if r := m[fsID(0)]; r.Kind != report.KindPVC || r.Facts["type"] != "file system" || r.Facts["replication"] != "to fb-dr-01, replicating, 4 min behind" ||
		r.Facts["used_gib"] != 200.0 || r.Facts["provisioned_gib"] != 1024.0 {
		t.Errorf("home %+v", r)
	}
	if r := m[fsID(2)]; r.Facts["replication"] != "to fb-dr-01, unhealthy, 5 h behind" {
		t.Errorf("scratch %+v", r.Facts)
	}
	if r := m["bk-1"]; r.Facts["type"] != "bucket" || r.Facts["account"] != "veeam" || r.Facts["replication"] != "to s3-offsite, paused" ||
		r.Facts["quota_gib"] != 102400.0 || r.Facts["used_gib"] != 40960.0 || r.Facts["versioning"] != "enabled" {
		t.Errorf("backups %+v", r.Facts)
	}
	if r := m["connection/c-2"]; r.Kind != report.KindCluster || r.Name != "connection/fb-lab-01" {
		t.Errorf("connection %+v", r)
	}
	us := Usage(s)
	if len(us) != 1 || us[0].IO == nil || us[0].IO.ReadOps != 120000 || us[0].IO.UsedOf != int64(2000)<<40 || us[0].IO.Used != int64(1300)<<40 {
		t.Errorf("usage %+v", us)
	}
	c.Logout(context.Background())
	if f.calls[len(f.calls)-1] != "POST /api/logout" || len(f.sessions) != 0 {
		t.Errorf("logout: %v", f.calls[len(f.calls)-1])
	}
}

func TestPartlyReadable(t *testing.T) {
	f := blade()
	f.fail["/buckets"] = http.StatusForbidden
	f.fail["/arrays/performance"] = http.StatusForbidden
	c := setup(t, f)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	self := Records(s)[0]
	if !strings.Contains(self.Word, "can't read buckets: flashblade: 403 Permission denied.") || Usage(s) != nil ||
		!strings.HasPrefix(fmt.Sprint(self.Facts["performance"]), "can't read it: flashblade: 403") {
		t.Errorf("the array %q %v", self.Word, self.Facts)
	}
	f.fail["/arrays"] = http.StatusForbidden
	if _, err := c.Read(context.Background(), time.Now()); err == nil {
		t.Error("read without the array")
	}
}

// A FlashBlade set up as a FlashArray (no type: flashblade) says so.
func TestWrongType(t *testing.T) {
	f := blade()
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "token"), []byte(f.token), 0o600)
	sum := sha256.Sum256(srv.Certificate().Raw)
	cfg := Config{URL: srv.URL, TokenFile: filepath.Join(dir, "token"), Fingerprint: hex.EncodeToString(sum[:])}
	cfg.Defaults()
	if cfg.Type != "flasharray" {
		t.Fatalf("default type %q", cfg.Type)
	}
	fa, err := newAs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fa.Read(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "(is it a flashblade? then say type: flashblade)") {
		t.Errorf("%v", err)
	}
}

func TestUnreachable(t *testing.T) {
	c := setup(t, blade())
	s, _ := c.Read(context.Background(), time.Now())
	last := Records(s)
	down := Unreachable("fb", last, errors.New("dial tcp: i/o timeout"))
	if len(down) != len(last) || down[0].State != report.StateDown || down[0].Name != "fb-prod-01" || down[0].Word != "Can't reach its API: dial tcp: i/o timeout" {
		t.Errorf("%+v", down[0])
	}
	if first := Unreachable("fb-new", nil, errors.New("refused")); len(first) != 1 || first[0].Facts["distribution"] != "Purity//FB" {
		t.Errorf("%+v", first)
	}
}
