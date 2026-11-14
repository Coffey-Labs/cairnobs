package akamai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Akamai's published EdgeGrid test vector (the edgegrid libraries'
// testdata: "simple GET").
func TestSignVector(t *testing.T) {
	c := Creds{Host: "akaa-baseurl-xxxxxxxxxxx-xxxxxxxxxxxxx.luna.akamaiapis.net", ClientToken: "akab-client-token-xxx-xxxxxxxxxxxxxxxx",
		ClientSecret: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=", AccessToken: "akab-access-token-xxx-xxxxxxxxxxxxxxxx"}
	if k := mac(c.ClientSecret, "20140321T19:34:21+0000"); k != "znsRMDBRqTXGJ7Ojip3/h2FGPu3LuoMYWgv9PKEnE/o=" {
		t.Errorf("signing key %s", k) // testdata.json's sign_key_test
	}
	r, _ := http.NewRequest(http.MethodGet, "https://"+c.Host+"/", nil)
	got := c.authorization(r, nil, "20140321T19:34:21+0000", "nonce-xx-xxxx-xxxx-xxxx-xxxxxxxxxxxx")
	want := "EG1-HMAC-SHA256 client_token=akab-client-token-xxx-xxxxxxxxxxxxxxxx;access_token=akab-access-token-xxx-xxxxxxxxxxxxxxxx;" +
		"timestamp=20140321T19:34:21+0000;nonce=nonce-xx-xxxx-xxxx-xxxx-xxxxxxxxxxxx;signature=tL+y4hxyHxgWVD30X3pWnGKHcPzmrIF+LThiAOhMxYU="
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// Akamai's published POST vectors (testcases.json: "POST inside limit",
// "POST too large" and "POST length equals max_body" with max_body 2048,
// "POST empty body").
func TestSignPOSTVectors(t *testing.T) {
	c := Creds{Host: "akaa-baseurl-xxxxxxxxxxx-xxxxxxxxxxxxx.luna.akamaiapis.net", ClientToken: "akab-client-token-xxx-xxxxxxxxxxxxxxxx",
		ClientSecret: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx=", AccessToken: "akab-access-token-xxx-xxxxxxxxxxxxxxxx", MaxBody: 2048}
	for _, v := range []struct {
		path, body, sig string
	}{
		{"/testapi/v1/t3", "datadatadatadatadatadatadatadata", "hXm4iCxtpN22m4cbZb4lVLW5rhX8Ca82vCFqXzSTPe4="},
		{"/testapi/v1/t3", strings.Repeat("d", 2049), "6Q6PiTipLae6n4GsSIDTCJ54bEbHUBp+4MUXrbQCBoY="},
		{"/testapi/v1/t3", strings.Repeat("d", 2048), "6Q6PiTipLae6n4GsSIDTCJ54bEbHUBp+4MUXrbQCBoY="},
		{"/testapi/v1/t6", "", "1gEDxeQGD5GovIkJJGcBaKnZ+VaPtrc4qBUHixjsPCQ="},
	} {
		r, _ := http.NewRequest(http.MethodPost, "https://"+c.Host+v.path, nil)
		got := c.authorization(r, []byte(v.body), "20140321T19:34:21+0000", "nonce-xx-xxxx-xxxx-xxxx-xxxxxxxxxxxx")
		if _, sig, _ := strings.Cut(got, "signature="); sig != v.sig {
			t.Errorf("%s (%d bytes): %s, want %s", v.path, len(v.body), sig, v.sig)
		}
	}
}

func writeEdgerc(t *testing.T, c Creds) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".edgerc")
	host := c.Host
	if !strings.HasPrefix(host, "http://") {
		host = "https://" + host + "/"
	}
	body := "# Akamai\n[other]\nhost = elsewhere\n\n[default]\nhost = " + host + "\nclient_token = " + c.ClientToken +
		"\nclient_secret = " + c.ClientSecret + "\naccess_token = " + c.AccessToken + "\nmax-body = 131072\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEdgerc(t *testing.T) {
	c := testCreds()
	p := writeEdgerc(t, c)
	got, err := ReadEdgerc(p, "default")
	c.MaxBody = 131072 // the file's max-body
	if err != nil || got != c {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadEdgerc(p, "missing"); err == nil || !strings.Contains(err.Error(), "no [missing] section") {
		t.Errorf("%v", err)
	}
	if _, err := ReadEdgerc(p, "other"); err == nil || !strings.Contains(err.Error(), "needs host, client_token") {
		t.Errorf("%v", err)
	}
}

func testCreds() Creds {
	return Creds{Host: "akab-test.luna.akamaiapis.net", ClientToken: "akab-ct", ClientSecret: "c2VjcmV0", AccessToken: "akab-at"}
}

// GTM, read from the fake: a property alive in both datacenters, one with
// a datacenter down, one with none alive; and a wrong signature refused.
func TestGTM(t *testing.T) {
	c := testCreds()
	f := &Fake{Creds: c}
	dcs := map[int]string{3131: "Winterfell", 3132: "Braavos"}
	f.SetDomain("example.akadns.net", FakeDomain{Type: "weighted", Datacenters: dcs, Properties: []FakeProperty{
		{Name: "www", Type: "failover", Targets: map[int]map[string]bool{3131: {"192.0.2.10": true}, 3132: {"198.51.100.10": true}}},
		{Name: "api", Type: "weighted-round-robin", Targets: map[int]map[string]bool{3131: {"192.0.2.20": true, "192.0.2.21": true}, 3132: {"198.51.100.20": false}}},
		{Name: "legacy", Type: "failover", Targets: map[int]map[string]bool{3131: {"192.0.2.30": false}}, Off: map[int]bool{}},
	}})
	f.SetDomain("pending.akadns.net", FakeDomain{Type: "basic", Propagation: "PENDING", Since: time.Now().Add(-time.Hour), Datacenters: dcs})
	srv := httptest.NewServer(f)
	defer srv.Close()

	w, err := New(Config{Edgerc: writeEdgerc(t, c), API: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w.Poll(context.Background(), now)
	s := w.Snapshot()
	if s.Error != "" || len(s.Domains) != 2 || s.Domains[0].Name != "example.akadns.net" || s.Domains[1].Propagation != "PENDING" {
		t.Fatalf("%+v", s)
	}
	props := map[string]Property{}
	for _, p := range s.Domains[0].Properties {
		props[p.Name] = p
	}
	live := func(p Property) (n int) {
		for _, t := range p.Targets {
			if t.Live() {
				n++
			}
		}
		return n
	}
	if p := props["www"]; live(p) != 2 || p.At.IsZero() || p.Error != "" {
		t.Errorf("www: %+v", p)
	}
	if p := props["api"]; live(p) != 1 || len(p.Targets) != 2 {
		t.Errorf("api: %+v", p)
	}
	if p := props["legacy"]; live(p) != 0 || !p.Targets[0].Servers[0].Known {
		t.Errorf("legacy: %+v", p)
	}

	// A wrong secret: the reading fails and the last good one stays.
	bad := c
	bad.ClientSecret = "d3Jvbmc="
	w.cfg.Edgerc = writeEdgerc(t, bad)
	w.Poll(context.Background(), now.Add(time.Minute))
	if s := w.Snapshot(); !strings.Contains(s.Error, "Not authorized") || len(s.Domains) != 2 || !s.ErrorSince.Equal(now.Add(time.Minute)) {
		t.Errorf("after a bad signature: %+v", s)
	}

	// No GTM in the account: said, not an error.
	f.NoGTM = true
	w.cfg.Edgerc = writeEdgerc(t, c)
	w.Poll(context.Background(), now.Add(2*time.Minute))
	if s := w.Snapshot(); s.Error != "" || len(s.Domains) != 0 || len(s.Absent) != 1 {
		t.Errorf("without GTM: %+v", s)
	}
}

// A stand-in may be spoken to in plain http on loopback, nowhere else.
func TestEdgercStandIn(t *testing.T) {
	c := testCreds()
	c.Host = "http://127.0.0.1:58400"
	got, err := ReadEdgerc(writeEdgerc(t, c), "default")
	if err != nil || !got.Plain || got.Host != "127.0.0.1:58400" {
		t.Errorf("%+v %v", got, err)
	}
	c.Host = "http://akab-x.luna.akamaiapis.net"
	if _, err := ReadEdgerc(writeEdgerc(t, c), "default"); err == nil {
		t.Error("plain http to a real host accepted")
	}
}
