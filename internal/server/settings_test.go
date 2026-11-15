package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func sendCookie(h http.Handler, method, path string, c *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if c != nil {
		req.AddCookie(c)
	}
	req.Header.Set("X-Beacon", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func devCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	return post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
}

func TestChecksEditedInTheUI(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	c := devCookie(t, h)

	if rec := get(h, "/api/v1/settings/checks", c); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"source":"ui"`) || !strings.Contains(rec.Body.String(), `"hosts":["node1"]`) {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/checks", c); !strings.Contains(rec.Body.String(), `"configured":true`) {
		t.Fatalf("checks page: %s", rec.Body)
	}
	rec := sendCookie(h, "PUT", "/api/v1/settings/checks/site", c, `{"kind":"http","target":"https://example.org/","name":"Site","from":["node1"]}`)
	if rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	work, ver := srv.workFor("node1")
	if ver == "" || len(work.Checks) != 1 || work.Checks[0].HTTP != "https://example.org/" || work.Checks[0].Status != 200 {
		t.Fatalf("agent work: %+v", work)
	}
	// Refused: an unknown host, two kinds' worth of nothing, a bad id, a short interval.
	for _, bad := range []struct{ path, body, want string }{
		{"/api/v1/settings/checks/x", `{"kind":"http","target":"https://example.org/","from":["node9"]}`, `No host called \"node9\"`},
		{"/api/v1/settings/checks/x", `{"kind":"http","target":""}`, "say what to check"},
		{"/api/v1/settings/checks/x", `{"kind":"smtp","target":"a"}`, "isn't http"},
		{"/api/v1/settings/checks/-x", `{"kind":"ping","target":"example.org"}`, "letters, digits"},
		{"/api/v1/settings/checks/x", `{"kind":"ping","target":"example.org","every_s":5}`, "at least 30s"},
	} {
		if rec := sendCookie(h, "PUT", bad.path, c, bad.body); rec.Code != 400 || !strings.Contains(rec.Body.String(), bad.want) {
			t.Errorf("%s: %d %s", bad.body, rec.Code, rec.Body)
		}
	}
	if rec := sendCookie(h, "PUT", "/api/v1/settings/checks/site", c, `{"kind":"tcp","target":"example.org:443","tls":true}`); rec.Code != 200 {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}

	// It survives a restart, exports as a checks file, and is audited.
	srv.Close()
	srv = stateServer(t, dir, true)
	h = srv.Handler()
	c = devCookie(t, h)
	rec = get(h, "/api/v1/settings/checks/export", c)
	if rec.Code != 200 || rec.Body.String() != "checks:\n  - id: site\n    tcp: example.org:443\n    tls: true\n" {
		t.Fatalf("export: %d %q", rec.Code, rec.Body)
	}
	if _, err := parseChecks(rec.Body.Bytes(), map[string]bool{"node1": true}); err != nil {
		t.Fatalf("the export isn't a checks file: %v", err)
	}
	audit := get(h, "/api/v1/audit", c).Body.String()
	for _, want := range []string{`"action":"added a check","detail":"site: http https://example.org/"`, `"action":"changed a check","detail":"site: tcp example.org:443"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %s", want)
		}
	}

	// Import replaces the lot, or nothing at all.
	if rec := sendCookie(h, "POST", "/api/v1/settings/checks/import", c, "checks:\n  - id: a\n    ping: example.org\n  - id: a\n    ping: example.net\n"); rec.Code != 400 || !strings.Contains(rec.Body.String(), `two checks called`) {
		t.Fatalf("bad import: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/checks/import", c, "checks:\n  - id: a\n    ping: example.org\n    colour: red\n"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "colour") {
		t.Fatalf("unknown key: %d %s", rec.Code, rec.Body)
	}
	if work, _ := srv.workFor("node1"); len(work.Checks) != 1 || work.Checks[0].ID != "site" {
		t.Fatalf("a refused import changed the checks: %+v", work.Checks)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/checks/import", c, "checks:\n  - id: a\n    ping: example.org\n  - id: b\n    dns: example.org\n    type: MX\n"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":2`) {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "DELETE", "/api/v1/settings/checks/a", c, ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if work, _ := srv.workFor("node1"); len(work.Checks) != 1 || work.Checks[0].ID != "b" {
		t.Fatalf("after delete: %+v", work.Checks)
	}
	if rec := sendCookie(h, "DELETE", "/api/v1/settings/checks/a", c, ""); rec.Code != 404 {
		t.Fatalf("delete again: %d", rec.Code)
	}
	if rec := sendCookie(h, "PUT", "/api/v1/settings/checks/x", nil, `{"kind":"ping","target":"example.org"}`); rec.Code != 401 {
		t.Fatalf("without a session: %d", rec.Code)
	}
}

func TestAChecksFileWins(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "checks.yaml")
	if err := os.WriteFile(f, []byte("checks:\n  - id: web\n    http: https://example.org/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	srv, err := New(&Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Checks: f,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key, Maintenance: nil,
		Agents: []AgentConfig{{Name: "node1", TokenSHA256: HashToken(NewAgentToken())}}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	c := devCookie(t, h)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/checks/x", c, `{"kind":"ping","target":"example.org"}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "checks.yaml; change them there") {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/checks/import", c, "checks: []\n"); rec.Code != 409 {
		t.Fatalf("import: %d", rec.Code)
	}
	if rec := get(h, "/api/v1/checks", c); !strings.Contains(rec.Body.String(), `"source":"file"`) {
		t.Fatalf("checks: %s", rec.Body)
	}
	// Export still works: what the file says, defaults left out.
	if rec := get(h, "/api/v1/settings/checks/export", c); !strings.Contains(rec.Body.String(), "http: https://example.org/") {
		t.Fatalf("export: %s", rec.Body)
	}
	// Maintenance windows aren't in the file, so they're the UI's.
	if rec := get(h, "/api/v1/settings/maintenance", c); !strings.Contains(rec.Body.String(), `"source":"ui"`) {
		t.Fatalf("maintenance: %s", rec.Body)
	}
}

func TestMaintenanceWindowsEditedInTheUI(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	c := devCookie(t, h)
	rec := sendCookie(h, "PUT", "/api/v1/settings/maintenance/node1%20reboot", c, `{"hosts":["node1"],"daily":"04:30","timezone":"Europe/Berlin","duration_s":1200}`)
	if rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if !srv.heldHosts(time.Date(2026, 11, 12, 4, 40, 0, 0, berlin))["node1"] || srv.heldHosts(time.Date(2026, 11, 12, 5, 0, 0, 0, berlin))["node1"] {
		t.Fatal("the window doesn't hold node1 when it should")
	}
	for _, bad := range []struct{ body, want string }{
		{`{"hosts":[],"daily":"04:30","duration_s":60}`, "names no hosts"},
		{`{"hosts":["node9"],"daily":"04:30","duration_s":60}`, `No host called \"node9\"`},
		{`{"hosts":["node1"],"daily":"4:30","duration_s":60}`, "HH:MM"},
		{`{"hosts":["node1"],"daily":"04:30","timezone":"Mars/Olympus","duration_s":60}`, "unknown time zone"},
		{`{"hosts":["node1"],"daily":"04:30","duration_s":86400}`, "between 1s and 12h"},
	} {
		if rec := sendCookie(h, "PUT", "/api/v1/settings/maintenance/w", c, bad.body); rec.Code != 400 || !strings.Contains(rec.Body.String(), bad.want) {
			t.Errorf("%s: %d %s", bad.body, rec.Code, rec.Body)
		}
	}

	srv.Close()
	srv = stateServer(t, dir, true)
	h = srv.Handler()
	c = devCookie(t, h)
	rec = get(h, "/api/v1/incidents", c)
	if !strings.Contains(rec.Body.String(), `"name":"node1 reboot"`) || !strings.Contains(rec.Body.String(), `"maintenance_source":"ui"`) {
		t.Fatalf("after restart: %s", rec.Body)
	}
	exp := get(h, "/api/v1/settings/maintenance/export", c).Body.String()
	if exp != "maintenance:\n  - name: node1 reboot\n    hosts:\n      - node1\n    daily: \"04:30\"\n    timezone: Europe/Berlin\n    duration: 20m0s\n" {
		t.Fatalf("export: %q", exp)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/maintenance/import", c, exp); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Fatalf("the export doesn't import: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "DELETE", "/api/v1/settings/maintenance/node1%20reboot", c, ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if srv.heldHosts(time.Date(2026, 11, 12, 4, 40, 0, 0, berlin))["node1"] {
		t.Fatal("a removed window still holds")
	}
	if !strings.Contains(get(h, "/api/v1/audit", c).Body.String(), `"action":"removed a maintenance window","detail":"node1 reboot"`) {
		t.Fatal("not audited")
	}
}

func TestMaintenanceInTheConfigFileWins(t *testing.T) {
	srv, _ := testServer(t, true)
	h := srv.Handler()
	c := devCookie(t, h)
	// testServer has no database: nothing can be kept.
	if rec := sendCookie(h, "PUT", "/api/v1/settings/checks/x", c, `{"kind":"ping","target":"example.org"}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "no database") {
		t.Fatalf("no database: %d %s", rec.Code, rec.Body)
	}

	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	cfgPath := filepath.Join(dir, "obs-beacond.yaml")
	_ = os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\ndev_signin: true\ndatabase: "+strconv.Quote(statetest.Database(t, dir))+"\ndata_dir: "+dir+"\nstate_key_file: "+key+"\nmaintenance: []\n"), 0o600)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Maintenance == nil {
		t.Fatal("an empty maintenance list in the file reads as absent")
	}
	srv, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h = srv.Handler()
	c = devCookie(t, h)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/maintenance/w", c, `{"hosts":["node1"],"daily":"04:30","duration_s":60}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "obs-beacond.yaml") {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
}
