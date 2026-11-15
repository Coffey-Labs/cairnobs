package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

const testOrigin = "http://localhost:8150"

// softKey is a software passkey: what a phone or security key does, in a
// few lines, so the whole ceremony can be tested.
type softKey struct {
	id   []byte
	key  *ecdsa.PrivateKey
	user []byte // the user handle it was made for
	n    uint32
}

var b64 = base64.RawURLEncoding

func rpHash() []byte { h := sha256.Sum256([]byte("localhost")); return h[:] }

// keyOrigin is where the software passkey thinks it is: the front door by default.
var keyOrigin = testOrigin

func clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": keyOrigin, "crossOrigin": false})
	return b
}

// create answers navigator.credentials.create() for the given options.
func create(t *testing.T, opts []byte) (*softKey, []byte) {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(opts, &o); err != nil {
		t.Fatal(err)
	}
	k := &softKey{id: make([]byte, 16)}
	_, _ = rand.Read(k.id)
	k.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k.user, _ = b64.DecodeString(o.PublicKey.User.ID)
	x, y := k.key.X.FillBytes(make([]byte, 32)), k.key.Y.FillBytes(make([]byte, 32))
	cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	var ad bytes.Buffer
	ad.Write(rpHash())
	ad.WriteByte(0x45) // user present, user verified, attested credential data
	_ = binary.Write(&ad, binary.BigEndian, uint32(0))
	ad.Write(make([]byte, 16)) // AAGUID
	_ = binary.Write(&ad, binary.BigEndian, uint16(len(k.id)))
	ad.Write(k.id)
	ad.Write(cose)
	att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad.Bytes()})
	resp, _ := json.Marshal(map[string]any{"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(clientData("webauthn.create", o.PublicKey.Challenge)), "attestationObject": b64.EncodeToString(att)}})
	return k, resp
}

// get answers navigator.credentials.get() for the given options.
func (k *softKey) get(t *testing.T, opts []byte) []byte {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(opts, &o); err != nil {
		t.Fatal(err)
	}
	k.n++
	var ad bytes.Buffer
	ad.Write(rpHash())
	ad.WriteByte(0x05) // user present, user verified
	_ = binary.Write(&ad, binary.BigEndian, k.n)
	cd := clientData("webauthn.get", o.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	sig, _ := ecdsa.SignASN1(rand.Reader, k.key, sha256Of(append(ad.Bytes(), h[:]...)))
	resp, _ := json.Marshal(map[string]any{"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(ad.Bytes()),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(k.user)}})
	return resp
}

func sha256Of(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func localServer(t *testing.T) *Server { return localServerWith(t, "") }

func localServerWith(t *testing.T, door string) *Server {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "state.key")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	_ = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k)), 0o600)
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key, EmergencyListen: door,
		Agents: []AgentConfig{{Name: "node1", TokenSHA256: HashToken(NewAgentToken())}},
		Signin: SigninConfig{Providers: []ProviderConfig{{ID: "local", Type: "local", Origin: testOrigin}}}}
	for i := range cfg.Signin.Providers {
		if err := cfg.Signin.Providers[i].check(); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// browser is a cookie jar over the handler.
type browser struct {
	h       http.Handler
	cookies map[string]*http.Cookie
}

func (b *browser) do(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Beacon", "1")
	req.Header.Set("Content-Type", "application/json")
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

func TestPasskeySetupSignInAndViewerLimits(t *testing.T) {
	srv := localServer(t)
	h := srv.Handler()
	admin := &browser{h: h, cookies: map[string]*http.Cookie{}}
	admin.do("POST", "/auth/dev", nil)

	// An admin adds a viewer and gets a setup link.
	rec := admin.do("POST", "/api/v1/users", []byte(`{"name":"Robin","email":"Robin@Example.test","role":"viewer"}`))
	var made struct {
		User struct{ ID, Email string } `json:"user"`
		Link string                     `json:"setup_link"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if rec.Code != 200 || made.User.Email != "robin@example.test" || !strings.HasPrefix(made.Link, testOrigin+"/setup#") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	token := strings.TrimPrefix(made.Link, testOrigin+"/setup#")
	if rec := admin.do("POST", "/api/v1/users", []byte(`{"name":"Again","email":"robin@example.test","role":"viewer"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate email: %d", rec.Code)
	}

	// Robin opens the link and registers a passkey.
	robin := &browser{h: h, cookies: map[string]*http.Cookie{}}
	if rec := robin.do("POST", "/auth/setup/info", []byte(`{"token":"`+token+`"}`)); !strings.Contains(rec.Body.String(), `"name":"Robin"`) {
		t.Fatalf("setup info: %s", rec.Body)
	}
	rec = robin.do("POST", "/auth/setup/begin", []byte(`{"token":"`+token+`"}`))
	if rec.Code != 200 {
		t.Fatalf("setup begin: %d %s", rec.Code, rec.Body)
	}
	key, att := create(t, rec.Body.Bytes())
	rec = robin.do("POST", "/auth/setup/finish?name=Laptop", att)
	if rec.Code != 200 || robin.cookies[sessionCookie] == nil {
		t.Fatalf("setup finish: %d %s", rec.Code, rec.Body)
	}
	if rec := robin.do("GET", "/api/v1/session", nil); !strings.Contains(rec.Body.String(), `"role":"viewer"`) || !strings.Contains(rec.Body.String(), `"user":"Robin"`) {
		t.Fatalf("session: %s", rec.Body)
	}
	// The link is spent.
	if rec := (&browser{h: h, cookies: map[string]*http.Cookie{}}).do("POST", "/auth/setup/begin", []byte(`{"token":"`+token+`"}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("link reused: %d", rec.Code)
	}

	// A viewer looks but doesn't touch.
	if rec := robin.do("GET", "/api/v1/overview", nil); rec.Code != 200 {
		t.Fatalf("viewer overview: %d", rec.Code)
	}
	for _, p := range [][2]string{{"POST", "/api/v1/alerts/test"}, {"POST", "/api/v1/incidents/B-0001/ack"}, {"GET", "/api/v1/users"}, {"POST", "/api/v1/users"}} {
		if rec := robin.do(p[0], p[1], []byte(`{}`)); rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s: %d", p[0], p[1], rec.Code)
		}
	}

	// Sign out, then back in with the passkey.
	robin.do("POST", "/auth/signout", nil)
	if rec := robin.do("GET", "/api/v1/session", nil); rec.Code != 401 || !strings.Contains(rec.Body.String(), `"kind":"passkey"`) {
		t.Fatalf("after sign-out: %d %s", rec.Code, rec.Body)
	}
	rec = robin.do("POST", "/auth/passkey/begin", nil)
	if rec.Code != 200 {
		t.Fatalf("passkey begin: %d %s", rec.Code, rec.Body)
	}
	if rec := robin.do("POST", "/auth/passkey/finish", key.get(t, rec.Body.Bytes())); rec.Code != 200 {
		t.Fatalf("passkey finish: %d %s", rec.Code, rec.Body)
	}
	if rec := robin.do("GET", "/api/v1/session", nil); !strings.Contains(rec.Body.String(), `"user":"Robin"`) {
		t.Fatalf("signed back in: %s", rec.Body)
	}

	// A replayed answer to the same challenge is refused (the ceremony is used up).
	rec = robin.do("POST", "/auth/passkey/begin", nil)
	answer := key.get(t, rec.Body.Bytes())
	robin.do("POST", "/auth/passkey/finish", answer)
	if rec := robin.do("POST", "/auth/passkey/finish", answer); rec.Code == 200 {
		t.Fatal("the same answer signed in twice")
	}

	// The admin disables the account: Robin's session ends on the next request.
	if rec := admin.do("POST", "/api/v1/users/"+made.User.ID, []byte(`{"disabled":true}`)); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if rec := robin.do("GET", "/api/v1/session", nil); rec.Code != 401 {
		t.Fatalf("disabled account still signed in: %d", rec.Code)
	}
	rec = robin.do("POST", "/auth/passkey/begin", nil)
	if rec := robin.do("POST", "/auth/passkey/finish", key.get(t, rec.Body.Bytes())); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled account signed in: %d %s", rec.Code, rec.Body)
	}

	// The admin's list shows the passkey; everything went in the audit log.
	rec = admin.do("GET", "/api/v1/users", nil)
	if !strings.Contains(rec.Body.String(), `"name":"Laptop"`) || !strings.Contains(rec.Body.String(), `"disabled":true`) || strings.Contains(rec.Body.String(), "credential") {
		t.Fatalf("users: %s", rec.Body)
	}
	audit := admin.do("GET", "/api/v1/audit", nil).Body.String()
	for _, want := range []string{"added a local account", "added a passkey", "with a passkey as viewer", "changed a local account", "the local account was disabled or removed"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %q", want)
		}
	}
}

func TestViewerDoesNotSeeLogs(t *testing.T) {
	srv := localServer(t)
	rows := []ContainerRow{{}}
	rows[0].Logs = []string{"secret-ish line"}
	req := httptest.NewRequest("GET", "/", nil)
	if got := srv.logsFor(req, rows); got[0].Logs != nil {
		t.Fatal("a request without an admin session got log lines")
	}
}

func TestLocalNeedsDatabaseAndOrigin(t *testing.T) {
	for _, o := range []string{"", "http://beacon.example.org", "https://beacon.example.org/path", "ftp://x"} {
		pc := ProviderConfig{ID: "local", Type: "local", Origin: o}
		if err := pc.check(); err == nil {
			t.Errorf("accepted origin %q", o)
		}
	}
	_, err := New(&Config{Listen: "127.0.0.1:0", Signin: SigninConfig{Providers: []ProviderConfig{{ID: "local", Type: "local", Origin: "https://beacon.example.org"}}}})
	if err == nil || !strings.Contains(err.Error(), "needs `database`") {
		t.Fatalf("no database: %v", err)
	}
}

func TestEmergencyDoor(t *testing.T) {
	srv := localServerWith(t, "127.0.0.1:8152")
	ch := &spy{id: "phone", min: "critical"}
	ops := &spy{id: "ops", min: "warning"}
	srv.alerts.channels = []notify.Channel{ch, ops}
	front, door := srv.Handler(), srv.EmergencyHandler()
	admin := &browser{h: front, cookies: map[string]*http.Cookie{}}
	admin.do("POST", "/auth/dev", nil)

	rec := admin.do("POST", "/api/v1/users", []byte(`{"name":"Break Glass","email":"glass@example.test","role":"viewer","emergency":true}`))
	var made struct {
		User struct {
			ID        string
			Role      string
			Emergency bool
		} `json:"user"`
		Link string `json:"setup_link"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if !made.User.Emergency || made.User.Role != "admin" || !strings.HasPrefix(made.Link, "http://localhost:8152/setup#") {
		t.Fatalf("emergency account: %s", rec.Body)
	}
	token := strings.TrimPrefix(made.Link, "http://localhost:8152/setup#")

	// The front door won't set it up.
	if rec := (&browser{h: front, cookies: map[string]*http.Cookie{}}).do("POST", "/auth/setup/info", []byte(`{"token":"`+token+`"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("front door setup: %d %s", rec.Code, rec.Body)
	}

	// Through the door it works, and everyone is told.
	keyOrigin = "http://localhost:8152"
	defer func() { keyOrigin = testOrigin }()
	glass := &browser{h: door, cookies: map[string]*http.Cookie{}}
	rec = glass.do("POST", "/auth/setup/begin", []byte(`{"token":"`+token+`"}`))
	key, att := create(t, rec.Body.Bytes())
	if rec := glass.do("POST", "/auth/setup/finish?name=Phone", att); rec.Code != 200 {
		t.Fatalf("door setup: %d %s", rec.Code, rec.Body)
	}
	glass.do("POST", "/auth/signout", nil)
	rec = glass.do("POST", "/auth/passkey/begin", nil)
	if rec := glass.do("POST", "/auth/passkey/finish", key.get(t, rec.Body.Bytes())); rec.Code != 200 {
		t.Fatalf("door sign-in: %d %s", rec.Code, rec.Body)
	}
	drain(srv.alerts)
	if len(ch.got) != 2 || len(ops.got) != 2 || !strings.HasPrefix(ch.got[1].Title, "Emergency sign-in: Break Glass") {
		t.Fatalf("not everyone was told: phone %+v ops %+v", ch.got, ops.got)
	}

	// The same passkey at the front door: refused (and no session).
	keyOrigin = testOrigin
	outside := &browser{h: front, cookies: map[string]*http.Cookie{}}
	rec = outside.do("POST", "/auth/passkey/begin", nil)
	if rec := outside.do("POST", "/auth/passkey/finish", key.get(t, rec.Body.Bytes())); rec.Code == 200 || outside.cookies[sessionCookie] != nil {
		t.Fatalf("emergency passkey worked at the front door: %d", rec.Code)
	}
	// An emergency account stays an admin.
	if rec := admin.do("POST", "/api/v1/users/"+made.User.ID, []byte(`{"role":"viewer"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("emergency demoted: %d", rec.Code)
	}
	audit := admin.do("GET", "/api/v1/audit", nil).Body.String()
	if !strings.Contains(audit, "signed in through the emergency door") || !strings.Contains(audit, "as emergency admin") {
		t.Fatalf("audit: %s", audit)
	}
}

func TestEmergencyDoorNeedsLoopbackAndDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obs-beacond.yaml")
	_ = os.WriteFile(p, []byte("emergency_listen: \"0.0.0.0:8152\"\n"), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("public emergency door: %v", err)
	}
	if _, err := New(&Config{Listen: "127.0.0.1:0", EmergencyListen: "127.0.0.1:8152"}); err == nil {
		t.Fatal("emergency door without the local directory")
	}
}
