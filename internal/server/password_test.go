package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
	"github.com/cairnobs/cairnobs/internal/totp"
)

// builtIn starts obs-beacond with no sign-in configured (dir keeps its
// database across calls) and returns it with what it logged.
func builtIn(t *testing.T, dir string) (*Server, string) {
	t.Helper()
	key := filepath.Join(dir, "state.key")
	if _, err := os.Stat(key); err != nil {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		_ = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k)), 0o600)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	srv, err := New(&Config{Listen: "127.0.0.1:8150", StaleAfter: time.Minute, Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, buf.String()
}

var firstLink = regexp.MustCompile(`http://127\.0\.0\.1:8150/setup#(first-[A-Za-z0-9_-]+)`)

func jsonOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

func TestFirstAdminWithPasswordAndCode(t *testing.T) {
	dir := t.TempDir()
	srv, logged := builtIn(t, dir)
	m := firstLink.FindStringSubmatch(logged)
	if m == nil {
		t.Fatalf("no first-admin link logged:\n%s", logged)
	}
	first := m[1]
	h := srv.Handler()
	b := &browser{h: h, cookies: map[string]*http.Cookie{}}

	// The sign-in page offers the built-in directory, passwords on, passkeys off.
	rec := b.do("GET", "/api/v1/session", nil)
	if !strings.Contains(rec.Body.String(), `"passkeys":false`) || !strings.Contains(rec.Body.String(), `"passwords":true`) {
		t.Fatalf("sign-in options: %s", rec.Body)
	}
	if rec := b.do("POST", "/auth/first", []byte(`{"token":"first-wrong","check":true}`)); rec.Code != 404 {
		t.Fatalf("a wrong link: %d", rec.Code)
	}
	if rec := b.do("POST", "/auth/first", []byte(`{"token":"`+first+`","check":true}`)); rec.Code != 200 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	rec = b.do("POST", "/auth/first", []byte(`{"token":"`+first+`","name":"Pat","email":"Pat@Example.test"}`))
	if rec.Code != 200 {
		t.Fatalf("first admin: %d %s", rec.Code, rec.Body)
	}
	setup := jsonOf(t, rec.Body.Bytes())["setup"].(string)

	info := jsonOf(t, b.do("POST", "/auth/setup/info", []byte(`{"token":"`+setup+`"}`)).Body.Bytes())
	if info["role"] != "admin" || info["passkeys"] != false || info["passwords"] != true {
		t.Fatalf("setup info: %v", info)
	}
	rec = b.do("POST", "/auth/setup/totp", []byte(`{"token":"`+setup+`"}`))
	secret, _ := jsonOf(t, rec.Body.Bytes())["secret"].(string)
	if rec.Code != 200 || secret == "" {
		t.Fatalf("totp: %d %s", rec.Code, rec.Body)
	}
	now := time.Now()
	code, _ := totp.Code(secret, totp.Step(now))
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"token":"` + setup + `","password":"short","code":"` + code + `"}`, 400},
		{`{"token":"` + setup + `","password":"correct horse battery","code":"000000"}`, 400},
		{`{"token":"` + setup + `","password":"pat@example.test","code":"` + code + `"}`, 400},
		{`{"token":"` + setup + `","password":"correct horse battery","code":"` + code + `"}`, 200},
		{`{"token":"` + setup + `","password":"correct horse battery again","code":"` + code + `"}`, 404}, // the link is used up
	} {
		if rec := b.do("POST", "/auth/setup/password", []byte(tc.body)); rec.Code != tc.want {
			t.Fatalf("%s: %d %s, want %d", tc.body, rec.Code, rec.Body, tc.want)
		}
		if tc.want == 200 {
			if s := jsonOf(t, b.do("GET", "/api/v1/session", nil).Body.Bytes()); s["role"] != "admin" {
				t.Fatalf("not signed in after setup: %v", s)
			}
		}
	}
	if rec := b.do("POST", "/auth/first", []byte(`{"token":"`+first+`","check":true}`)); rec.Code != 404 {
		t.Fatalf("the first-admin link still works: %d", rec.Code)
	}

	// Sign in afresh: the setup's code can't be used again; the next one can.
	c := &browser{h: h, cookies: map[string]*http.Cookie{}}
	signin := func(pw, code string) int {
		return c.do("POST", "/auth/password", []byte(`{"email":" PAT@example.test","password":"`+pw+`","code":"`+code+`"}`)).Code
	}
	if got := signin("correct horse battery", code); got != 401 {
		t.Fatalf("the setup code worked again: %d", got)
	}
	next, _ := totp.Code(secret, totp.Step(now)+1)
	if got := signin("wrong horse battery", next); got != 401 {
		t.Fatalf("a wrong password: %d", got)
	}
	if got := signin("correct horse battery", next); got != 200 {
		t.Fatalf("sign in: %d", got)
	}
	if got := signin("correct horse battery", next); got != 401 {
		t.Fatalf("one code, two sign-ins: %d", got)
	}

	// A restart with an admin logs no link.
	srv.Close()
	srv2, logged := builtIn(t, dir)
	if strings.Contains(logged, "setup#first-") {
		t.Fatalf("logged a first-admin link with an admin there:\n%s", logged)
	}
	_ = srv2
}

func TestPasswordSigninIsLimited(t *testing.T) {
	srv, logged := builtIn(t, t.TempDir())
	h := srv.Handler()
	b := &browser{h: h, cookies: map[string]*http.Cookie{}}
	first := firstLink.FindStringSubmatch(logged)[1]
	setup := jsonOf(t, b.do("POST", "/auth/first", []byte(`{"token":"`+first+`","name":"Pat","email":"pat@example.test"}`)).Body.Bytes())["setup"].(string)
	secret := jsonOf(t, b.do("POST", "/auth/setup/totp", []byte(`{"token":"`+setup+`"}`)).Body.Bytes())["secret"].(string)
	code, _ := totp.Code(secret, totp.Step(time.Now())-1)
	if rec := b.do("POST", "/auth/setup/password", []byte(`{"token":"`+setup+`","password":"correct horse battery","code":"`+code+`"}`)); rec.Code != 200 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	c := &browser{h: h, cookies: map[string]*http.Cookie{}}
	for i := 0; i < maxFails; i++ {
		c.do("POST", "/auth/password", []byte(`{"email":"pat@example.test","password":"nope nope nope","code":"123456"}`))
	}
	good, _ := totp.Code(secret, totp.Step(time.Now()))
	if rec := c.do("POST", "/auth/password", []byte(`{"email":"pat@example.test","password":"correct horse battery","code":"`+good+`"}`)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", maxFails, rec.Code)
	}

	// An admin removes the password: it no longer signs in.
	_ = srv.db.ClearSigninFailures("acct:pat@example.test")
	_ = srv.db.ClearSigninFailures("ip:192.0.2.1") // httptest's address
	u, _ := srv.db.UserByEmail("pat@example.test")
	if rec := b.do("DELETE", "/api/v1/users/"+u.ID+"/password", nil); rec.Code != 204 {
		t.Fatalf("remove password: %d %s", rec.Code, rec.Body)
	}
	next, _ := totp.Code(secret, totp.Step(time.Now())+1)
	if rec := c.do("POST", "/auth/password", []byte(`{"email":"pat@example.test","password":"correct horse battery","code":"`+next+`"}`)); rec.Code != 401 {
		t.Fatalf("signed in after the password was removed: %d", rec.Code)
	}
}

func TestAbandonedFirstAdminGetsANewLink(t *testing.T) {
	dir := t.TempDir()
	srv, logged := builtIn(t, dir)
	b := &browser{h: srv.Handler(), cookies: map[string]*http.Cookie{}}
	first := firstLink.FindStringSubmatch(logged)[1]
	if rec := b.do("POST", "/auth/first", []byte(`{"token":"`+first+`","name":"Pat","email":"pat@example.test"}`)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	// Never finished: the next start logs a new link, and the same email
	// takes over the unfinished account.
	srv.Close()
	srv2, logged := builtIn(t, dir)
	m := firstLink.FindStringSubmatch(logged)
	if m == nil {
		t.Fatal("no new link after an unfinished first admin")
	}
	b2 := &browser{h: srv2.Handler(), cookies: map[string]*http.Cookie{}}
	if rec := b2.do("POST", "/auth/first", []byte(`{"token":"`+m[1]+`","name":"Pat Again","email":"pat@example.test"}`)); rec.Code != 200 {
		t.Fatalf("again: %d %s", rec.Code, rec.Body)
	}
	if us, _ := srv2.db.Users(); len(us) != 1 || us[0].Name != "Pat Again" {
		t.Fatalf("users: %+v", us)
	}
}

func TestConfiguredDirectoryHasNoPasswordsUnlessAsked(t *testing.T) {
	srv := localServer(t)
	b := &browser{h: srv.Handler(), cookies: map[string]*http.Cookie{}}
	if rec := b.do("POST", "/auth/password", []byte(`{"email":"a@b.test","password":"x","code":"1"}`)); rec.Code != 404 {
		t.Fatalf("password sign-in on a passkeys-only directory: %d", rec.Code)
	}
	if strings.Contains(b.do("GET", "/api/v1/session", nil).Body.String(), `"passwords":true`) {
		t.Fatal("offered passwords")
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash %s", h)
	}
	if !checkPassword(h, "correct horse battery") || checkPassword(h, "correct horse batterY") || checkPassword("", "") || checkPassword("$argon2id$v=19$m=99999999,t=1,p=1$AA$AA", "x") {
		t.Fatal("checkPassword")
	}
}
