package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func TestAdoptWhatsRunning(t *testing.T) {
	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	c := devCookie(t, h)
	now := time.Now()
	srv.store.Put("host1", report.Report{V: report.Version, Time: now,
		Containers: &report.Containers{List: []report.Container{
			{Name: "web", State: "running"}, {Name: "db", State: "running"}, {Name: "old", State: "exited"},
			{Name: "GITEA-ACTIONS-TASK-1", State: "running"},
		}},
		Services: []string{"sshd.service", "docker.service", "app.service"}}, now)

	rec := get(h, "/api/v1/settings/expected/hosts/host1", c)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"source":"ui"`) || !strings.Contains(body, `"services":["app.service","docker.service","sshd.service"]`) ||
		!strings.Contains(body, `{"name":"web","state":"running","status":""}`) {
		t.Fatalf("settings: %d %s", rec.Code, body)
	}
	// Before anything is expected, an unknown container is just running.
	if row := containerRow(t, srv, "host1", "web"); row.Verdict.Word != "Running" {
		t.Fatalf("before: %+v", row.Verdict)
	}

	rec = sendCookie(h, "PUT", "/api/v1/settings/expected/hosts/host1", c,
		`{"running":["web"," db ","web"],"stopped":[{"match":"old","note":"retired"}],"units":["app.service"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"running":["web","db"]`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if row := containerRow(t, srv, "host1", "old"); row.Verdict.Word != "Stopped on purpose" {
		t.Fatalf("old: %+v", row.Verdict)
	}
	// The unit reaches the agent with its work.
	if work, ver := srv.workFor("host1"); ver == "" || len(work.Units) != 1 || work.Units[0] != "app.service" {
		t.Fatalf("work: %+v", work)
	}
	// A container that turns up later is now "Not expected".
	srv.store.Put("host1", report.Report{V: report.Version, Time: now, Containers: &report.Containers{List: []report.Container{
		{Name: "web", State: "running"}, {Name: "db", State: "running"}, {Name: "new", State: "running"}}}}, now)
	if row := containerRow(t, srv, "host1", "new"); row.Verdict.Word != "Not expected" {
		t.Fatalf("new: %+v", row.Verdict)
	}
	// An agent from before services were reported: null, not [].
	if body := get(h, "/api/v1/settings/expected/hosts/host1", c).Body.String(); !strings.Contains(body, `"services":null`) {
		t.Fatalf("no services: %s", body)
	}

	for _, bad := range []struct{ body, want string }{
		{`{"running":["web["]}`, "bad pattern"},
		{`{"units":["app service"]}`, "isn't a systemd unit name"},
		{`{"running":["web"],"stopped":[{"match":"web"}]}`, "both expected running and stopped"},
	} {
		if rec := sendCookie(h, "PUT", "/api/v1/settings/expected/hosts/host1", c, bad.body); rec.Code != 400 || !strings.Contains(rec.Body.String(), bad.want) {
			t.Errorf("%s: %d %s", bad.body, rec.Code, rec.Body)
		}
	}
	if rec := sendCookie(h, "PUT", "/api/v1/settings/expected/hosts/host9", c, `{}`); rec.Code != 404 {
		t.Fatalf("unknown host: %d", rec.Code)
	}

	// It survives a restart, and exports as expected.yaml.
	srv.Close()
	srv = stateServer(t, dir, true)
	h = srv.Handler()
	c = devCookie(t, h)
	exp := get(h, "/api/v1/settings/expected/export", c).Body.String()
	want := "hosts:\n  host1:\n    running:\n      - web\n      - db\n    stopped:\n      - match: old\n        note: retired\n    units:\n      - app.service\n"
	if exp != want {
		t.Fatalf("export:\n%s", exp)
	}
	if _, err := parseExpected([]byte(exp)); err != nil {
		t.Fatal(err)
	}
	// Import replaces it all, or nothing.
	if rec := sendCookie(h, "POST", "/api/v1/settings/expected/import", c, "hosts:\n  host9:\n    running: [x]\n"); rec.Code != 400 || !strings.Contains(rec.Body.String(), `no host called \"host9\"`) {
		t.Fatalf("bad import: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/expected/import", c, "ci_jobs: [RUNNER-*]\nhosts:\n  host1:\n    running: [web*]\n"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	// Emptying a host takes it out: judged on Docker's state alone again.
	if rec := sendCookie(h, "PUT", "/api/v1/settings/expected/hosts/host1", c, `{}`); rec.Code != 200 {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	if exp := get(h, "/api/v1/settings/expected/export", c).Body.String(); exp != "ci_jobs:\n  - RUNNER-*\n" {
		t.Fatalf("after emptying:\n%s", exp)
	}
	audit := get(h, "/api/v1/audit", c).Body.String()
	for _, w := range []string{`"action":"set what should run on host1","detail":"2 containers running, 1 container stopped on purpose, 1 service"`, `"action":"imported what should run where"`} {
		if !strings.Contains(audit, w) {
			t.Errorf("audit lacks %s", w)
		}
	}
}

func TestAnExpectedFileWins(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "expected.yaml")
	_ = os.WriteFile(f, []byte("hosts:\n  host1:\n    running: [web]\n"), 0o600)
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	srv, err := New(&Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Expected: f,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []AgentConfig{{Name: "host1", TokenSHA256: HashToken(NewAgentToken())}}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	c := devCookie(t, h)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/expected/hosts/host1", c, `{"running":["x"]}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "expected.yaml; change them there") {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if body := get(h, "/api/v1/settings/expected/hosts/host1", c).Body.String(); !strings.Contains(body, `"source":"file"`) || !strings.Contains(body, `"running":["web"]`) {
		t.Fatalf("get: %s", body)
	}
}

func containerRow(t *testing.T, srv *Server, host, name string) ContainerRow {
	t.Helper()
	hosts, _ := srv.view(time.Now())
	for _, h := range hosts {
		if h.Name == host {
			for _, r := range h.rows {
				if r.Name == name {
					return r
				}
			}
		}
	}
	t.Fatalf("no container %s on %s", name, host)
	return ContainerRow{}
}
