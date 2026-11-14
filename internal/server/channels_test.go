package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// ntfyCatcher records what reaches a fake ntfy server.
type ntfyCatcher struct {
	mu   sync.Mutex
	hits []string // path and Authorization
}

func (n *ntfyCatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.hits = append(n.hits, r.URL.Path+" "+r.Header.Get("Authorization"))
	n.mu.Unlock()
}

func (n *ntfyCatcher) last() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.hits) == 0 {
		return ""
	}
	return n.hits[len(n.hits)-1]
}

func TestChannelsEditedInTheUI(t *testing.T) {
	catch := &ntfyCatcher{}
	ntfy := httptest.NewServer(catch)
	defer ntfy.Close()

	dir := t.TempDir()
	srv := stateServer(t, dir, true)
	h := srv.Handler()
	c := devCookie(t, h)
	const topic, token = "cobs-topic-k3Jq9", "tk_s3cr3t-value"

	if rec := get(h, "/api/v1/settings/channels", c); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"source":"ui"`) {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}
	rec := sendCookie(h, "PUT", "/api/v1/settings/channels/ops", c, `{"type":"ntfy","min":"warning","server":"`+ntfy.URL+`","secrets":{"topic":"`+topic+`","token":"`+token+`"}}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), topic) || !strings.Contains(rec.Body.String(), `"set":["topic","token"]`) {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/alerts/test?channel=ops", c, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"result":"sent"`) {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := catch.last(); got != "/"+topic+" Bearer "+token {
		t.Fatalf("ntfy got %q", got)
	}

	// Editing without the secrets keeps them; clearing the token drops it.
	if rec := sendCookie(h, "PUT", "/api/v1/settings/channels/ops", c, `{"type":"ntfy","min":"serious","server":"`+ntfy.URL+`","clear":["token","topic"]}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"set":["topic"]`) {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	sendCookie(h, "POST", "/api/v1/alerts/test", c, "")
	if got := catch.last(); got != "/"+topic+" " {
		t.Fatalf("after the change ntfy got %q", got)
	}

	// Refused, without repeating a secret.
	for _, bad := range []struct{ id, body, want string }{
		{"d", `{"type":"discord","secrets":{"webhook":"not-a-url-hunter2"}}`, "the webhook isn't an http(s) URL"},
		{"d", `{"type":"discord"}`, "webhook is missing"},
		{"d", `{"type":"discord","secrets":{"topic":"x"}}`, "A discord channel has no topic"},
		{"d", `{"type":"pager"}`, "isn't ntfy"},
		{"-d", `{"type":"log"}`, "letters, digits"},
		{"ops", `{"type":"ntfy","min":"loud","server":"` + ntfy.URL + `"}`, `isn't warning, serious or critical`},
	} {
		rec := sendCookie(h, "PUT", "/api/v1/settings/channels/"+bad.id, c, bad.body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), bad.want) || strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("%s: %d %s", bad.body, rec.Code, rec.Body)
		}
	}

	// An email channel is fine next to ntfy, but ntfy can't then go, or
	// critical alerts would only go out by email.
	if rec := sendCookie(h, "PUT", "/api/v1/settings/channels/mail", c, `{"type":"email","smtp":"smtp.example.org:587","from":"Cairn OBS <obs@example.org>","to":["ops@example.org"],"secrets":{"password":"pw-hunter2"}}`); rec.Code != 200 {
		t.Fatalf("email: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "DELETE", "/api/v1/settings/channels/ops", c, ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "only go out by email") {
		t.Fatalf("delete ntfy: %d %s", rec.Code, rec.Body)
	}

	// Sealed in the database: no secret in the stored text.
	s, ok, err := srv.db.Setting(sectionChannels)
	if err != nil || !ok || strings.Contains(s.YAML, topic) || strings.Contains(s.YAML, "hunter2") || strings.Contains(s.YAML, "ops") {
		t.Fatalf("stored: %v %v %q", ok, err, s.YAML)
	}

	// They survive a restart; export leaves the secrets out, and importing
	// it back keeps them.
	srv.Close()
	srv = stateServer(t, dir, true)
	h = srv.Handler()
	c = devCookie(t, h)
	if n := len(srv.alerts.chans()); n != 2 {
		t.Fatalf("%d channels after a restart", n)
	}
	rec = get(h, "/api/v1/settings/channels/export", c)
	exp := rec.Body.String()
	if rec.Code != 200 || strings.Contains(exp, topic) || strings.Contains(exp, "hunter2") || !strings.Contains(exp, "id: ops") || !strings.Contains(exp, "min: serious") || !strings.HasPrefix(exp, "# Secrets aren't exported") {
		t.Fatalf("export: %d %q", rec.Code, exp)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/channels/import", c, exp); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":2`) {
		t.Fatalf("import the export: %d %s", rec.Code, rec.Body)
	}
	sendCookie(h, "POST", "/api/v1/alerts/test?channel=ops", c, "")
	if got := catch.last(); got != "/"+topic+" " {
		t.Fatalf("after the import ntfy got %q", got)
	}
	// A channel new to the import needs its secret; *_file isn't allowed.
	if rec := sendCookie(h, "POST", "/api/v1/settings/channels/import", c, "channels:\n  - id: hook\n    type: webhook\n"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "url is missing") {
		t.Fatalf("import without a secret: %d %s", rec.Code, rec.Body)
	}
	if rec := sendCookie(h, "POST", "/api/v1/settings/channels/import", c, "channels:\n  - id: hook\n    type: webhook\n    url_file: /etc/shadow\n"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "only work in obs-beacond.yaml") {
		t.Fatalf("import with a file: %d %s", rec.Code, rec.Body)
	}
	if n := len(srv.alerts.chans()); n != 2 {
		t.Fatalf("a refused import changed the channels: %d", n)
	}

	audit := get(h, "/api/v1/audit", c).Body.String()
	for _, want := range []string{`"detail":"ops: ntfy, warning and up"`, `"detail":"ops: ntfy, serious and up; new token"`, `"action":"imported the alert channels"`, `"action":"sent a test alert","detail":"ops"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
	if strings.Contains(audit, topic) || strings.Contains(audit, "hunter2") {
		t.Error("a secret reached the audit log")
	}
	if rec := sendCookie(h, "PUT", "/api/v1/settings/channels/x", nil, `{"type":"log"}`); rec.Code != 401 {
		t.Fatalf("without a session: %d", rec.Code)
	}
}

func TestChannelsInTheConfigFileWin(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	topicFile := filepath.Join(dir, "topic")
	_ = os.WriteFile(topicFile, []byte("file-topic-hunter2\n"), 0o600)
	cfgPath := filepath.Join(dir, "obs-beacond.yaml")
	_ = os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\ndev_signin: true\ndatabase: "+strconv.Quote(statetest.Database(t, dir))+"\ndata_dir: "+dir+"\nstate_key_file: "+key+
		"\nalerts:\n  channels:\n    - id: phone\n      type: ntfy\n      topic_file: "+topicFile+"\n"), 0o600)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	c := devCookie(t, h)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/channels/x", c, `{"type":"log"}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "obs-beacond.yaml; change them there") {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/incidents", c); !strings.Contains(rec.Body.String(), `"channels_source":"file"`) {
		t.Fatalf("incidents: %s", rec.Body)
	}
	// Export says what the file does, without the secret or where it lives.
	rec := get(h, "/api/v1/settings/channels/export", c)
	if !strings.Contains(rec.Body.String(), "id: phone") || strings.Contains(rec.Body.String(), "topic") {
		t.Fatalf("export: %s", rec.Body)
	}
}
