package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/cloudflare"
)

func cfServer(t *testing.T, s cloudflare.Snapshot) *Server {
	t.Helper()
	srv, _ := testServer(t, true)
	tf := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tf, []byte("x"), 0o600)
	w, err := cloudflare.New(cloudflare.Config{TokenFile: tf, SSLModeZones: map[string]string{"tenant.example.com": "full"}})
	if err != nil {
		t.Fatal(err)
	}
	w.Set(s)
	srv.cf = w
	return srv
}

func keys(c []Condition) string {
	var k []string
	for _, x := range c {
		k = append(k, x.Severity+":"+x.Key)
	}
	return strings.Join(k, " ")
}

func TestCloudflareConditions(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	srv := cfServer(t, cloudflare.Snapshot{
		Checked: now,
		Zones: []cloudflare.Zone{
			{Name: "example.org", SSL: "strict", Certs: []cloudflare.Cert{{Status: "active", Hosts: []string{"example.org"}, Expires: now.Add(60 * day)}}},
			{Name: "example.test", SSL: "full", Certs: []cloudflare.Cert{{Status: "active", Hosts: []string{"example.test"}, Expires: now.Add(10 * day)}}},
			{Name: "ihasvpn.org", SSL: "strict", Certs: []cloudflare.Cert{{Status: "active", Hosts: []string{"ihasvpn.org"}, Expires: now.Add(36 * time.Hour)}}},
			{Name: "tenant.example.com", SSL: "full", Certs: []cloudflare.Cert{{Status: "pending_validation", Hosts: []string{"tenant.example.com"}}}},
		},
		Pools: []cloudflare.Pool{
			{Name: "web", Enabled: true, Healthy: true, Origins: []cloudflare.Origin{
				{Name: "host1", Enabled: true, Healthy: true},
				{Name: "host2", Enabled: true, Healthy: false, Reason: "TCP connection failed (seen from ENAM)"},
				{Name: "host3", Enabled: false, Healthy: false},
			}},
			{Name: "mail", Enabled: true, Healthy: false},
			{Name: "old", Enabled: false, Healthy: false},
		},
		Status: cloudflare.Status{Indicator: "major", Description: "Partial System Outage"},
	})
	got := keys(srv.cloudflareConditions(now))
	want := "warning:cloudflare/origin/web/host2 critical:cloudflare/pool/mail " +
		"warning:cloudflare/ssl/example.test warning:cloudflare/cert/example.test/example.test " +
		"critical:cloudflare/cert/ihasvpn.org/ihasvpn.org warning:cloudflare/cert/tenant.example.com/tenant.example.com"
	if got != want {
		t.Fatalf("conditions\n got  %s\n want %s", got, want)
	}
	// They reach the alert engine through view(), criticals first.
	_, conds := srv.view(now)
	if !strings.Contains(keys(conds), "cloudflare/pool/mail") {
		t.Fatalf("view: %s", keys(conds))
	}
}

func TestCloudflareOutageWaitsThreePolls(t *testing.T) {
	now := time.Now()
	srv := cfServer(t, cloudflare.Snapshot{Checked: now.Add(-20 * time.Minute), Error: "cloudflare /zones: Internal error (code 10000)", ErrorSince: now.Add(-10 * time.Minute)})
	if c := srv.cloudflareConditions(now); len(c) != 0 {
		t.Fatalf("too early: %s", keys(c))
	}
	if c := srv.cloudflareConditions(now.Add(6 * time.Minute)); keys(c) != "warning:cloudflare/api" {
		t.Fatalf("after 16 minutes: %s", keys(c))
	}
}

func TestCloudflareAPI(t *testing.T) {
	srv, _ := testServer(t, true)
	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	if rec := get(h, "/api/v1/cloudflare", cookie); !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("unconfigured: %s", rec.Body)
	}
	srv = cfServer(t, cloudflare.Snapshot{Checked: time.Now(), Zones: []cloudflare.Zone{}, Pools: []cloudflare.Pool{}})
	h = srv.Handler()
	cookie = post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	rec := get(h, "/api/v1/cloudflare", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"conditions":[]`) || !strings.Contains(rec.Body.String(), `"ssl_mode":"strict"`) {
		t.Fatalf("configured: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/api/v1/cloudflare", nil); rec.Code != 401 {
		t.Fatalf("without a session: %d", rec.Code)
	}
}

func TestCloudflareTraffic(t *testing.T) {
	now := time.Now()
	zone := func(name string, tr cloudflare.Traffic) cloudflare.Zone {
		return cloudflare.Zone{Name: name, SSL: "strict", Traffic: &tr}
	}
	srv := cfServer(t, cloudflare.Snapshot{Checked: now, Zones: []cloudflare.Zone{
		zone("quiet.org", cloudflare.Traffic{Requests: 40, Errors: 10}),                                                                         // 25% but only 10 errors: no
		zone("warn.org", cloudflare.Traffic{Requests: 1000, Errors: 60, ErrorHosts: []cloudflare.HostCount{{Host: "git.warn.org", Count: 50}}}), // 6%
		zone("down.org", cloudflare.Traffic{Requests: 200, Errors: 120}),                                                                        // 60%
		zone("attack.org", cloudflare.Traffic{Requests: 5000, Blocked: 900, BlockedDay: 960}),                                                   // usual 10 per 15 min
		zone("noisy.org", cloudflare.Traffic{Requests: 5000, Blocked: 900, BlockedDay: 96 * 200}),                                               // always this busy
	}})
	conds := srv.cloudflareConditions(now)
	if got, want := keys(conds), "warning:cloudflare/errors/warn.org critical:cloudflare/errors/down.org warning:cloudflare/blocks/attack.org"; got != want {
		t.Fatalf("\n got  %s\n want %s", got, want)
	}
	if !strings.Contains(conds[0].Detail, "most on git.warn.org (50)") || conds[0].Title != "warn.org: 6% of requests failing" {
		t.Fatalf("detail: %+v", conds[0])
	}
}

func TestDNSDriftSurvivesRestartAndAccept(t *testing.T) {
	dir := t.TempDir()
	a := cloudflare.Record{Type: "A", Name: "obs.example.org", Content: "192.0.2.3", Proxied: true, TTL: 1}
	b := cloudflare.Record{Type: "A", Name: "www.example.org", Content: "192.0.2.1", Proxied: true, TTL: 1}
	tf := filepath.Join(dir, "cf-token")
	_ = os.WriteFile(tf, []byte("x"), 0o600)
	withCF := func(recs ...cloudflare.Record) *Server {
		srv := stateServer(t, dir, true)
		w, _ := cloudflare.New(cloudflare.Config{TokenFile: tf})
		w.Set(cloudflare.Snapshot{Checked: time.Now(), Zones: []cloudflare.Zone{{Name: "example.org", SSL: "strict", Records: recs}}})
		srv.cf = w
		return srv
	}
	srv := withCF(a, b)
	if c := srv.cloudflareConditions(time.Now()); len(c) != 0 {
		t.Fatalf("first sight is the baseline: %s", keys(c))
	}
	srv.Close()

	// www moved while obs-beacond was down.
	moved := b
	moved.Content = "198.51.100.7"
	srv = withCF(a, moved)
	c := srv.cloudflareConditions(time.Now())
	if keys(c) != "warning:cloudflare/dns/example.org" || c[0].Title != "DNS for example.org changed (1 added, 1 removed)" ||
		!strings.Contains(c[0].Detail, "removed A www.example.org → 192.0.2.1 (proxied); added A www.example.org → 198.51.100.7 (proxied)") {
		t.Fatalf("drift: %+v", c)
	}

	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	if rec := get(h, "/api/v1/cloudflare", cookie); !strings.Contains(rec.Body.String(), `"dns_changes":{"example.org":{"added":["A www.example.org → 198.51.100.7 (proxied)"]`) {
		t.Fatalf("api: %s", rec.Body)
	}
	if rec := postCookie(h, "/api/v1/cloudflare/dns/example.org/accept", cookie); rec.Code != 200 || rec.Body.String() != `{"added":1,"removed":1}`+"\n" {
		t.Fatalf("accept: %d %q", rec.Code, rec.Body)
	}
	if c := srv.cloudflareConditions(time.Now()); len(c) != 0 {
		t.Fatalf("after accept: %s", keys(c))
	}
	if rec := get(h, "/api/v1/audit", cookie); !strings.Contains(rec.Body.String(), `"action":"accepted DNS changes","detail":"example.org: 1 added, 1 removed"`) {
		t.Fatalf("audit: %s", rec.Body)
	}
	if rec := postCookie(h, "/api/v1/cloudflare/dns/nope.org/accept", cookie); rec.Code != 404 {
		t.Fatalf("unknown zone: %d", rec.Code)
	}
	srv.Close()
	// Accepted stays accepted after a restart.
	if c := withCF(a, moved).cloudflareConditions(time.Now()); len(c) != 0 {
		t.Fatalf("after accept and restart: %s", keys(c))
	}
}
