package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/akamai"
	"github.com/cairnobs/cairnobs/internal/report"
)

func akServer(t *testing.T, s akamai.Snapshot) *Server {
	t.Helper()
	srv, _ := testServer(t, true)
	w, err := akamai.New(akamai.Config{Edgerc: "/nonexistent/.edgerc"})
	if err != nil {
		t.Fatal(err)
	}
	w.Set(s)
	srv.ak = w
	return srv
}

func TestAkamaiConditions(t *testing.T) {
	now := time.Now()
	sv := func(ip string, alive bool) akamai.Server { return akamai.Server{IP: ip, Alive: alive, Known: true} }
	srv := akServer(t, akamai.Snapshot{Checked: now, Domains: []akamai.Domain{
		{Name: "example.akadns.net", Propagation: "COMPLETE", Properties: []akamai.Property{
			{Name: "www", At: now, Targets: []akamai.Target{
				{Datacenter: "Winterfell", Enabled: true, Servers: []akamai.Server{sv("192.0.2.10", true)}},
				{Datacenter: "Braavos", Enabled: true, Servers: []akamai.Server{sv("198.51.100.10", false)}},
			}},
			{Name: "legacy", At: now, Targets: []akamai.Target{
				{Datacenter: "Winterfell", Enabled: true, Servers: []akamai.Server{sv("192.0.2.30", false)}},
				{Datacenter: "Braavos", Enabled: false, Servers: []akamai.Server{sv("198.51.100.30", true)}},
			}},
			{Name: "new", Targets: []akamai.Target{{Datacenter: "Winterfell", Enabled: true, Servers: []akamai.Server{{IP: "192.0.2.40"}}}}},
		}},
		{Name: "pending.akadns.net", Propagation: "PENDING", Since: now.Add(-45 * time.Minute)},
		{Name: "fresh.akadns.net", Propagation: "PENDING", Since: now.Add(-5 * time.Minute)},
		{Name: "denied.akadns.net", Propagation: "DENIED", Message: "Validation failed."},
	}})
	// Braavos's server is host7's address.
	r := results(now)
	r.Map = &report.Map{Interfaces: []report.Interface{{Name: "eth0", Addrs: []string{"198.51.100.10/24"}}}}
	srv.store.Put("host7", r, now)

	got := srv.akamaiConditions(now)
	want := "warning:akamai/server/www.example.akadns.net/198.51.100.10 warning:akamai/server/legacy.example.akadns.net/192.0.2.30 " +
		"critical:akamai/property/legacy.example.akadns.net warning:akamai/propagation/pending.akadns.net warning:akamai/propagation/denied.akadns.net"
	if keys(got) != want {
		t.Fatalf("conditions\n got  %s\n want %s", keys(got), want)
	}
	if got[0].Host != "host7" || got[1].Host != "" || got[2].Detail != "Every datacenter's servers are failing GTM's liveness tests: Winterfell (192.0.2.30)." ||
		got[3].Title != "A change to pending.akadns.net has been propagating for 45 minutes" {
		t.Errorf("%+v", got)
	}
	if _, conds := srv.view(now); !strings.Contains(keys(conds), "akamai/property/legacy.example.akadns.net") {
		t.Errorf("view: %s", keys(conds))
	}

	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	if rec := get(h, "/api/v1/akamai", cookie); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hosts":{"198.51.100.10":"host7"}`) {
		t.Errorf("API: %d %s", rec.Code, rec.Body)
	}
}

// An unreadable API warns after three polls; the last reading stays.
func TestAkamaiOutage(t *testing.T) {
	now := time.Now()
	srv := akServer(t, akamai.Snapshot{Checked: now.Add(-time.Hour), Error: "connection refused", ErrorSince: now.Add(-10 * time.Minute)})
	if keys(srv.akamaiConditions(now)) != "" {
		t.Error("warned before three polls")
	}
	srv.ak.Set(akamai.Snapshot{Checked: now.Add(-time.Hour), Error: "connection refused", ErrorSince: now.Add(-16 * time.Minute)})
	if keys(srv.akamaiConditions(now)) != "warning:akamai/api" {
		t.Errorf("%s", keys(srv.akamaiConditions(now)))
	}
}

// Step 2: CPS certificates ending, a renewal waiting on the customer, and
// the deployed certificates on the Certificates page.
func TestAkamaiCPS(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cert := func(cn, fp, key string, left time.Duration) report.Cert {
		return report.Cert{Subject: cn, Names: []string{cn}, Issuer: "Let's Encrypt R11", Key: key, Fingerprint: fp, NotBefore: now.Add(-60 * day), NotAfter: now.Add(left)}
	}
	srv := akServer(t, akamai.Snapshot{Checked: now, Domains: []akamai.Domain{}, Enrollments: []akamai.Enrollment{
		{ID: 1001, CN: "www.example.com", Validation: "dv", Deployed: []report.Cert{cert("www.example.com", "aa", "RSA 2048", 60*day), cert("www.example.com", "ab", "ECDSA P-256", 60*day)}},
		{ID: 1002, CN: "shop.example.com", Validation: "third-party", Pending: 1, Waiting: "third-party-csr", Deployed: []report.Cert{cert("shop.example.com", "bb", "RSA 2048", 10*day)}},
		{ID: 1003, CN: "api.example.com", Validation: "dv", Deployed: []report.Cert{cert("api.example.com", "cc", "RSA 2048", 3*day)}},
		{ID: 1004, CN: "new.example.com", Validation: "dv", Pending: 1, Deployed: []report.Cert{}},
	}})
	got := srv.akamaiConditions(now)
	if k := keys(got); k != "warning:akamai/cps/1002/bb warning:akamai/cps/1002/waiting critical:akamai/cps/1003/cc" {
		t.Fatalf("%s", k)
	}
	if !strings.Contains(got[0].Detail, "third-party certificate") || !strings.Contains(got[1].Detail, "signed by your certificate authority") ||
		!strings.Contains(got[2].Detail, "stalled") || !strings.HasPrefix(got[2].Title, "Akamai certificate for api.example.com: ends in 3 days") {
		t.Errorf("%+v", got)
	}
	rows := srv.certificates(now)
	var akamaiRows int
	for _, r := range rows {
		if len(r.Places) == 1 && r.Places[0].Kind == "akamai" && r.Places[0].Href == "/akamai" {
			akamaiRows++
		}
	}
	if akamaiRows != 4 || rows[0].Places[0].Where != "CPS enrollment 1003 (api.example.com)" {
		t.Errorf("%d rows, first %+v", akamaiRows, rows[0].Places)
	}
}

// Step 3: a failed activation, one pending over an hour, and settled ones.
func TestAkamaiActivations(t *testing.T) {
	now := time.Now()
	act := func(v int, st string, ago time.Duration) *akamai.Activation {
		return &akamai.Activation{Version: v, Type: "ACTIVATE", Status: st, Submitted: now.Add(-ago)}
	}
	srv := akServer(t, akamai.Snapshot{Checked: now, Domains: []akamai.Domain{}, PMProperties: []akamai.PMProperty{
		{ID: "prp_1", Name: "www.example.com", Latest: 12, Staging: 12, Production: 12, LastStaging: act(12, "ACTIVE", 25*time.Hour), LastProduction: act(12, "ACTIVE", 24*time.Hour)},
		{ID: "prp_2", Name: "api.example.com", Latest: 7, Staging: 7, Production: 6, LastStaging: act(7, "ACTIVE", 3*time.Hour), LastProduction: act(7, "FAILED", 2*time.Hour)},
		{ID: "prp_3", Name: "media.example.com", Latest: 3, Staging: 2, LastStaging: act(3, "ZONE_2", 90*time.Minute)},
		{ID: "prp_4", Name: "new.example.com", Latest: 1, LastProduction: act(1, "PENDING", 20*time.Minute)},
	}})
	srv.ak.Snapshot().PMProperties[1].LastProduction.Note = "new origin"
	got := srv.akamaiConditions(now)
	if k := keys(got); k != "warning:akamai/activation/prp_2/production warning:akamai/activation/prp_3/staging" {
		t.Fatalf("%s", k)
	}
	if got[0].Title != "Akamai: activating api.example.com v7 on production failed" || !strings.Contains(got[0].Detail, "v6 stays active") || !strings.Contains(got[0].Detail, "Its note: new origin.") {
		t.Errorf("%+v", got[0])
	}
	if !strings.HasPrefix(got[1].Title, "Akamai: activating media.example.com v3 on staging has been pending for 90 minutes") || !strings.Contains(got[1].Detail, "ZONE_2") {
		t.Errorf("%+v", got[1])
	}
}

// Step 4: Edge DNS drift against the accepted baseline, accepting it, and
// the page not sent the record sets.
func TestAkamaiDNS(t *testing.T) {
	now := time.Now()
	a := akamai.RecordSet{Name: "www.example.com", Type: "CNAME", TTL: 300, RData: []string{"www.example.com.edgekey.net."}}
	mx := akamai.RecordSet{Name: "example.com", Type: "MX", TTL: 3600, RData: []string{"10 mx1.example.com."}}
	srv := akServer(t, akamai.Snapshot{Checked: now, Domains: []akamai.Domain{}, DNSZones: []akamai.DNSZone{
		{Name: "example.com", Type: "PRIMARY", Version: "v1", Records: []akamai.RecordSet{a, mx}},
		{Name: "alias.example", Type: "ALIAS"},
	}})
	if k := keys(srv.akamaiConditions(now)); k != "" {
		t.Fatalf("first reading is the baseline: %s", k)
	}
	mx2 := mx
	mx2.RData = []string{"10 mx9.example.net."}
	srv.ak.Set(akamai.Snapshot{Checked: now, Domains: []akamai.Domain{}, DNSZones: []akamai.DNSZone{{Name: "example.com", Version: "v2", Records: []akamai.RecordSet{a, mx2}}}})
	got := srv.akamaiConditions(now)
	if keys(got) != "warning:akamai/dns/example.com" || got[0].Title != "Akamai Edge DNS for example.com changed (1 added, 1 removed)" ||
		got[0].Detail != "removed MX example.com → 10 mx1.example.com. (TTL 3600); added MX example.com → 10 mx9.example.net. (TTL 3600). Accept it on the Akamai page if it was meant." {
		t.Fatalf("%+v", got)
	}

	h := srv.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/akamai", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rec, req)
	var page struct {
		Snapshot   akamai.Snapshot                `json:"snapshot"`
		DNSChanges map[string]map[string][]string `json:"dns_changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if z := page.Snapshot.DNSZones[0]; z.Records != nil || z.Count != 2 || len(page.DNSChanges["example.com"]["added"]) != 1 {
		t.Errorf("%+v %+v", z, page.DNSChanges)
	}
	if len(srv.ak.Snapshot().DNSZones[0].Records) != 2 {
		t.Error("the page's copy emptied the watcher's reading")
	}
	if r := postCookie(h, "/api/v1/akamai/dns/example.com/accept", cookie); r.Code != 200 || r.Body.String() != "{\"added\":1,\"removed\":1}\n" {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if k := keys(srv.akamaiConditions(now)); k != "" {
		t.Errorf("after accepting: %s", k)
	}
}

// Step 5: a CP code's 5xx share, with the minimum count.
func TestAkamaiTraffic(t *testing.T) {
	now := time.Now()
	srv := akServer(t, akamai.Snapshot{Checked: now, Domains: []akamai.Domain{}, TrafficStart: now.Add(-15 * time.Minute), TrafficEnd: now.Add(-10 * time.Minute),
		Traffic: []akamai.CPTraffic{{CPCode: 1001, Hits: 100000, Errors: 50}, {CPCode: 1002, Hits: 2000, Errors: 600}, {CPCode: 1003, Hits: 400, Errors: 40}, {CPCode: 1004, Hits: 30, Errors: 15}},
		Status:  akamai.Status{Indicator: "major", Description: "Partial System Outage"}})
	got := srv.akamaiConditions(now)
	if k := keys(got); k != "critical:akamai/traffic/1002 warning:akamai/traffic/1003" {
		t.Fatalf("%s", k)
	}
	if got[0].Title != "Akamai: 30% of CP code 1002's requests are failing with 5xx" || !strings.HasPrefix(got[0].Detail, "600 of 2000 edge hits") {
		t.Errorf("%+v", got[0])
	}
}
