package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const token = "test-token-not-real"

// Fake answers like the real API, from the shapes seen on 2026-10-15.
func Fake(t *testing.T, host2Healthy bool, fail *bool) *httptest.Server {
	t.Helper()
	ok := func(w http.ResponseWriter, result string, pages ...int) {
		info := ""
		if len(pages) == 2 {
			info = fmt.Sprintf(`,"result_info":{"page":%d,"total_pages":%d}`, pages[0], pages[1])
		}
		fmt.Fprintf(w, `{"success":true,"errors":[],"result":%s%s}`, result, info)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status.json" { // public, no token
			if r.Header.Get("Authorization") != "" {
				t.Error("the API token was sent to the status page")
			}
			fmt.Fprint(w, `{"status":{"indicator":"minor","description":"Minor Service Outage"}}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)
			return
		}
		if fail != nil && *fail {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Internal error"}]}`)
			return
		}
		page := r.URL.Query().Get("page")
		switch p := r.URL.Path; {
		case p == "/zones" && page == "1":
			ok(w, `[{"id":"z1","name":"example.org","account":{"id":"a1"}}]`, 1, 2)
		case p == "/zones" && page == "2":
			ok(w, `[{"id":"z2","name":"example.test","account":{"id":"a1"}}]`, 2, 2)
		case p == "/zones/z1/settings/ssl":
			ok(w, `{"id":"ssl","value":"strict"}`)
		case p == "/zones/z2/settings/ssl":
			ok(w, `{"id":"ssl","value":"full"}`)
		case p == "/zones/z1/ssl/certificate_packs":
			exp := time.Now().Add(50 * 24 * time.Hour).UTC().Format(time.RFC3339)
			ok(w, `[{"type":"universal","status":"active","hosts":["example.org","*.example.org"],"certificates":[{"expires_on":"`+exp+`"}]}]`, 1, 1)
		case p == "/zones/z2/ssl/certificate_packs":
			exp := time.Now().Add(2*24*time.Hour + time.Hour).UTC().Format(time.RFC3339)
			ok(w, `[{"type":"universal","status":"active","hosts":["example.test","*.example.test"],"certificates":[{"expires_on":"`+exp+`"}]}]`, 1, 1)
		case p == "/zones/z1/load_balancers":
			ok(w, `[{"name":"example.org"}]`, 1, 1)
		case p == "/zones/z2/load_balancers":
			ok(w, `[]`, 1, 1)
		case p == "/zones/z1/dns_records" && page == "1":
			ok(w, `[{"id":"r1","type":"A","name":"obs.example.org","content":"192.0.2.3","proxied":true,"ttl":1,"modified_on":"2026-10-15T00:00:00Z"}]`, 1, 2)
		case p == "/zones/z1/dns_records" && page == "2":
			ok(w, `[{"id":"r2","type":"MX","name":"example.org","content":"mx.example.net","proxied":false,"ttl":3600,"priority":10}]`, 2, 2)
		case p == "/zones/z2/dns_records":
			ok(w, `[]`, 1, 1)
		case p == "/graphql":
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !strings.Contains(body.Query, "httpRequestsAdaptiveGroups") || body.Variables["a"] == nil ||
				fmt.Sprint(body.Variables["blocking"]) != "[block challenge jschallenge managed_challenge connection_close force_connection_close]" {
				t.Errorf("graphql query: %+v", body)
			}
			// z1 had a bad quarter of an hour: 30 of 200 failed on git, and a burst of blocks
			// (15 blocked, 5 challenged). The decoy links AI Labyrinth added and the
			// already-passed challenges were served, so they are not blocks.
			fmt.Fprint(w, `{"data":{"viewer":{"zones":[
				{"zoneTag":"z1","win":[
					{"count":150,"dimensions":{"edgeResponseStatus":200,"securityAction":"unknown","clientRequestHTTPHost":"example.org"}},
					{"count":20,"dimensions":{"edgeResponseStatus":502,"securityAction":"unknown","clientRequestHTTPHost":"git.example.org"}},
					{"count":10,"dimensions":{"edgeResponseStatus":503,"securityAction":"unknown","clientRequestHTTPHost":"example.org"}},
					{"count":15,"dimensions":{"edgeResponseStatus":403,"securityAction":"block","clientRequestHTTPHost":"example.org"}},
					{"count":5,"dimensions":{"edgeResponseStatus":403,"securityAction":"managed_challenge","clientRequestHTTPHost":"example.org"}},
					{"count":700,"dimensions":{"edgeResponseStatus":301,"securityAction":"link_maze_injected","clientRequestHTTPHost":"example.org"}},
					{"count":9,"dimensions":{"edgeResponseStatus":200,"securityAction":"managed_challenge_bypassed","clientRequestHTTPHost":"example.org"}}],
				 "day":[{"count":96}]},
				{"zoneTag":"z2","win":[],"day":[]}]}},"errors":null}`)
		case p == "/accounts/a1/load_balancers/pools":
			ok(w, fmt.Sprintf(`[{"id":"p1","name":"main-web","enabled":true,"healthy":true,"origins":[
				{"name":"node1","address":"192.0.2.1","enabled":true},
				{"name":"node2","address":"192.0.2.2","enabled":true},
				{"name":"node3","address":"192.0.2.3","enabled":false}]},
				{"id":"p2","name":"spare","enabled":true,"healthy":%v,"origins":[]}]`, host2Healthy), 1, 1)
		case p == "/accounts/a1/load_balancers/pools/p1/health":
			ok(w, fmt.Sprintf(`{"pool_id":"p1","pop_health":{
				"WEU":{"healthy":true,"origins":[{"192.0.2.1":{"healthy":true,"rtt":"37.7ms","failure_reason":"No failures","response_code":200}},{"192.0.2.2":{"healthy":true,"rtt":"41ms","failure_reason":"No failures","response_code":200}}]},
				"ENAM":{"healthy":true,"origins":[{"192.0.2.1":{"healthy":true,"rtt":"90ms","failure_reason":"No failures","response_code":200}},{"192.0.2.2":{"healthy":%v,"rtt":"0ms","failure_reason":"TCP connection failed","response_code":0}}]}}}`, host2Healthy))
		case p == "/accounts/a1/load_balancers/pools/p2/health":
			ok(w, `{"pool_id":"p2","pop_health":{}}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func watcher(t *testing.T, api string, tok string) *Watcher {
	t.Helper()
	tf := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tf, []byte(tok+"\n"), 0o600)
	w, err := New(Config{TokenFile: tf, API: api, StatusURL: api + "/status.json"})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestReadsEverything(t *testing.T) {
	api := Fake(t, false, nil)
	w := watcher(t, api.URL, token)
	w.Poll(context.Background(), time.Now())
	s := w.Snapshot()
	if s.Error != "" || s.Checked.IsZero() {
		t.Fatalf("poll: %q", s.Error)
	}
	if len(s.Zones) != 2 || s.Zones[0].Name != "example.org" || s.Zones[1].SSL != "full" {
		t.Fatalf("zones (both pages): %+v", s.Zones)
	}
	if c := s.Zones[0].Certs; len(c) != 1 || c[0].Expires.IsZero() || len(s.Zones[0].LBs) != 1 {
		t.Fatalf("certs/lbs: %+v", s.Zones[0])
	}
	if len(s.Pools) != 2 || s.Pools[0].Name != "main-web" {
		t.Fatalf("pools: %+v", s.Pools)
	}
	o := s.Pools[0].Origins
	if !o[0].Healthy || o[1].Healthy || !strings.Contains(o[1].Reason, "TCP connection failed (seen from ENAM)") || o[2].Enabled {
		t.Fatalf("origins: %+v", o)
	}
	if s.Pools[1].Healthy || s.Status.Indicator != "minor" {
		t.Fatalf("spare pool / status: %+v %+v", s.Pools[1], s.Status)
	}
	z := s.Zones[0]
	if z.DNS != 2 || len(z.Records) != 2 || z.Records[1].Priority == nil || *z.Records[1].Priority != 10 {
		t.Fatalf("dns records (both pages): %+v", z.Records)
	}
	if got := z.Records[0].String(); got != "A obs.example.org → 192.0.2.3 (proxied)" {
		t.Fatalf("record text %q", got)
	}
	tr := z.Traffic
	if s.TrafficError != "" || tr == nil || tr.Requests != 909 || tr.Errors != 30 || tr.Blocked != 20 || tr.BlockedDay != 96 ||
		tr.ErrorHosts[0] != (HostCount{"git.example.org", 20}) {
		t.Fatalf("traffic: %+v %q", tr, s.TrafficError)
	}
	if s.Zones[1].Traffic == nil || s.Zones[1].Traffic.Requests != 0 {
		t.Fatalf("quiet zone traffic: %+v", s.Zones[1].Traffic)
	}
}

func TestZoneFilter(t *testing.T) {
	api := Fake(t, true, nil)
	w := watcher(t, api.URL, token)
	w.cfg.Zones = []string{"example.test"}
	w.Poll(context.Background(), time.Now())
	if s := w.Snapshot(); len(s.Zones) != 1 || s.Zones[0].Name != "example.test" {
		t.Fatalf("filtered zones: %+v %s", s.Zones, s.Error)
	}
}

func TestFailureKeepsLastReadingAndHidesToken(t *testing.T) {
	failing := false
	api := Fake(t, true, &failing)
	w := watcher(t, api.URL, token)
	t0 := time.Now()
	w.Poll(context.Background(), t0)
	failing = true
	w.Poll(context.Background(), t0.Add(5*time.Minute))
	w.Poll(context.Background(), t0.Add(10*time.Minute))
	s := w.Snapshot()
	if len(s.Zones) != 2 || !s.Checked.Equal(t0) {
		t.Fatal("a failed poll threw away the last good reading")
	}
	if !strings.Contains(s.Error, "Internal error") || !s.ErrorSince.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("error %q since %v", s.Error, s.ErrorSince)
	}
	failing = false
	w.Poll(context.Background(), t0.Add(15*time.Minute))
	if s := w.Snapshot(); s.Error != "" || !s.ErrorSince.IsZero() {
		t.Fatalf("recovery: %+v", s)
	}

	bad := watcher(t, api.URL, "wrong-token-value")
	bad.Poll(context.Background(), t0)
	if e := bad.Snapshot().Error; !strings.Contains(e, "Invalid access token") || strings.Contains(e, "wrong-token-value") {
		t.Fatalf("bad token error: %q", e)
	}
}

func TestMissingTokenRefused(t *testing.T) {
	if _, err := New(Config{TokenFile: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("started without a token")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("started without token_file")
	}
}

func TestStandInOnLoopbackOnly(t *testing.T) {
	tf := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tf, []byte(token+"\n"), 0o600)
	for _, bad := range []string{"https://127.0.0.1:1", "http://192.0.2.1:80", "http://api.example.org", "http://127.0.0.1:1/client/v4", "127.0.0.1:1"} {
		if _, err := New(Config{TokenFile: tf, StandIn: bad}); err == nil {
			t.Errorf("stand_in %s accepted", bad)
		}
	}
	srv := Fake(t, true, nil)
	w, err := New(Config{TokenFile: tf, StandIn: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	w.Poll(context.Background(), time.Now())
	if s := w.Snapshot(); s.Error != "" || len(s.Zones) != 2 || s.Status.Indicator != "minor" {
		t.Fatalf("through the stand-in: %+v", s)
	}
}
