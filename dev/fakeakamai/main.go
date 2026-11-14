// fakeakamai runs a stand-in Akamai API for local development
// (docs/akamai.md): a few GTM domains, one property failing over and one
// down, CPS certificates, Property Manager properties, Edge DNS zones (one
// drifting after a while), traffic by CP code, a status page, and an
// .edgerc pointing at it. Never deploy it.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cairnobs/cairnobs/internal/akamai"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:58400", "address (loopback only)")
	edgerc := flag.String("edgerc", ".edgerc", "where to write the .edgerc obs-beacond reads")
	driftAfter := flag.Duration("drift-after", 20*time.Minute, "change a DNS record this long after starting (0: never)")
	flag.Parse()
	c := akamai.Creds{Host: *listen, ClientToken: "akab-dev-client", ClientSecret: "ZGV2LXNlY3JldA==", AccessToken: "akab-dev-access"}
	body := fmt.Sprintf("[default]\nhost = http://%s\nclient_token = %s\nclient_secret = %s\naccess_token = %s\n", c.Host, c.ClientToken, c.ClientSecret, c.AccessToken)
	if err := os.WriteFile(*edgerc, []byte(body), 0o600); err != nil {
		log.Fatal(err)
	}
	f := &akamai.Fake{Creds: c}
	Populate(f)
	if *driftAfter > 0 {
		time.AfterFunc(*driftAfter, func() { drift(f) })
	}
	log.Printf("fake Akamai on %s, .edgerc in %s (development only)", *listen, *edgerc)
	srv := &http.Server{Addr: *listen, Handler: f, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// Populate gives the fake an enterprise's GTM: two domains over three
// datacenters, a property failing over, one down, a change propagating.
func Populate(f *akamai.Fake) {
	dcs := map[int]string{3131: "Ashburn", 3132: "Frankfurt", 3133: "Singapore"}
	f.SetDomain("shop.akadns.net", akamai.FakeDomain{Type: "full", Datacenters: dcs, Properties: []akamai.FakeProperty{
		{Name: "www", Type: "performance", Targets: map[int]map[string]bool{
			3131: {"203.0.113.10": true, "203.0.113.11": true}, 3132: {"198.51.100.10": true, "198.51.100.11": true}, 3133: {"192.0.2.10": true}}},
		{Name: "api", Type: "failover", Targets: map[int]map[string]bool{3131: {"203.0.113.20": false}, 3132: {"198.51.100.20": true}}},
		{Name: "checkout", Type: "weighted-round-robin", Targets: map[int]map[string]bool{3131: {"203.0.113.30": true}, 3132: {"198.51.100.30": true}},
			Off: map[int]bool{3132: true}},
	}})
	f.SetDomain("corp.akadns.net", akamai.FakeDomain{Type: "weighted", Propagation: "PENDING", Since: time.Now().Add(-50 * time.Minute), Datacenters: dcs,
		Properties: []akamai.FakeProperty{
			{Name: "vpn", Type: "failover", Targets: map[int]map[string]bool{3131: {"203.0.113.40": true}, 3132: {"198.51.100.40": true}}},
			{Name: "legacy", Type: "failover", Targets: map[int]map[string]bool{3131: {"203.0.113.50": false}}},
		}})
	// CPS: DV certificates with RSA and ECDSA, one whose renewal stalled,
	// a third-party one waiting on its CSR, one not deployed yet.
	day := 24 * time.Hour
	le := "Let's Encrypt R11"
	both := func(issuer string, names []string, end time.Time) []string {
		return []string{akamai.FakePEM(issuer, names, false, end), akamai.FakePEM(issuer, names, true, end)}
	}
	f.SetEnrollment(110201, akamai.FakeEnrollment{CN: "www.shop.example", SANs: []string{"www.shop.example", "shop.example"}, Validation: "dv", Contract: "1-ABCDE",
		Deployed: both(le, []string{"www.shop.example", "shop.example"}, time.Now().Add(61*day)), Waiting: "-"})
	f.SetEnrollment(110202, akamai.FakeEnrollment{CN: "api.shop.example", Validation: "dv", Contract: "1-ABCDE",
		Deployed: []string{akamai.FakePEM(le, []string{"api.shop.example"}, false, time.Now().Add(5*day))}, Waiting: "-"})
	f.SetEnrollment(110203, akamai.FakeEnrollment{CN: "checkout.shop.example", Validation: "third-party", Contract: "1-ABCDE",
		Deployed: []string{akamai.FakePEM("DigiCert Global G2 TLS RSA SHA256 2020 CA1", []string{"checkout.shop.example"}, false, time.Now().Add(16*day))}, Waiting: "third-party-csr"})
	f.SetEnrollment(110204, akamai.FakeEnrollment{CN: "*.corp.example", SANs: []string{"*.corp.example", "corp.example"}, Validation: "ov", Contract: "1-ABCDE",
		Deployed: both("DigiCert Global G2 TLS RSA SHA256 2020 CA1", []string{"*.corp.example", "corp.example"}, time.Now().Add(200*day)), Waiting: "-"})
	f.SetEnrollment(110205, akamai.FakeEnrollment{CN: "status.shop.example", Validation: "dv", Contract: "1-ABCDE"})
	// Property Manager: settled properties, a production activation that
	// failed, one stuck on staging, one rolling out now.
	at := func(ago time.Duration) time.Time { return time.Now().Add(-ago).UTC().Truncate(time.Second) }
	settled := func(name, group string, v int) akamai.FakePMProperty {
		return akamai.FakePMProperty{Name: name, Group: group, Contract: "1-ABCDE", Latest: v, Staging: v, Production: v, Activations: []akamai.FakeActivation{
			{Version: v, Network: "STAGING", Status: "ACTIVE", Submitted: at(time.Duration(v) * 26 * time.Hour)},
			{Version: v, Network: "PRODUCTION", Status: "ACTIVE", Submitted: at(time.Duration(v) * 25 * time.Hour)}}}
	}
	f.SetPMProperty("prp_810001", settled("www.shop.example", "Shop", 41))
	f.SetPMProperty("prp_810002", settled("static.shop.example", "Shop", 9))
	f.SetPMProperty("prp_810003", settled("corp.example", "Corporate", 17))
	api := settled("api.shop.example", "Shop", 22)
	api.Latest, api.Staging = 23, 23
	api.Activations = append(api.Activations,
		akamai.FakeActivation{Version: 23, Network: "STAGING", Status: "ACTIVE", Submitted: at(5 * time.Hour)},
		akamai.FakeActivation{Version: 23, Network: "PRODUCTION", Status: "FAILED", Submitted: at(3 * time.Hour), Note: "Move /v2 to the new origin"})
	f.SetPMProperty("prp_810004", api)
	media := settled("media.corp.example", "Corporate", 5)
	media.Latest = 6
	media.Activations = append(media.Activations, akamai.FakeActivation{Version: 6, Network: "STAGING", Status: "ZONE_1", Submitted: at(95 * time.Minute)})
	f.SetPMProperty("prp_810005", media)
	checkout := settled("checkout.shop.example", "Shop", 30)
	checkout.Latest, checkout.Staging = 31, 31
	checkout.Activations = append(checkout.Activations,
		akamai.FakeActivation{Version: 31, Network: "STAGING", Status: "ACTIVE", Submitted: at(50 * time.Minute)},
		akamai.FakeActivation{Version: 31, Network: "PRODUCTION", Status: "PENDING", Submitted: at(6 * time.Minute)})
	f.SetPMProperty("prp_810006", checkout)
	// Edge DNS: a few zones, one big.
	f.SetZone("shop.example", akamai.FakeZone{Records: shopZone("10 mx1.shop.example.")})
	f.SetZone("corp.example", akamai.FakeZone{Records: []akamai.RecordSet{
		{Name: "corp.example", Type: "A", TTL: 300, RData: []string{"203.0.113.80"}},
		{Name: "vpn.corp.example", Type: "CNAME", TTL: 300, RData: []string{"vpn.corp.akadns.net."}},
		{Name: "corp.example", Type: "TXT", TTL: 3600, RData: []string{`"v=spf1 mx -all"`}},
	}})
	var hosts []akamai.RecordSet
	for i := range 1500 {
		hosts = append(hosts, akamai.RecordSet{Name: fmt.Sprintf("node%04d.lab.example", i), Type: "A", TTL: 300, RData: []string{fmt.Sprintf("10.20.%d.%d", i/250, i%250+1)}})
	}
	f.SetZone("lab.example", akamai.FakeZone{Records: hosts})
	// Traffic: the shop's CP codes, the API's failing at 12%.
	f.SetTraffic(711001, 1_840_000, 310)
	f.SetTraffic(711002, 420_000, 50_400)
	f.SetTraffic(711003, 96_000, 12)
	f.SetTraffic(711004, 12_500, 0)
	f.Status = "minor"
}

// shopZone is shop.example's records, with the given MX.
func shopZone(mx string) []akamai.RecordSet {
	return []akamai.RecordSet{
		{Name: "www.shop.example", Type: "CNAME", TTL: 300, RData: []string{"www.shop.example.edgekey.net."}},
		{Name: "api.shop.example", Type: "CNAME", TTL: 300, RData: []string{"api.shop.akadns.net."}},
		{Name: "shop.example", Type: "MX", TTL: 3600, RData: []string{mx}},
		{Name: "_dmarc.shop.example", Type: "TXT", TTL: 3600, RData: []string{`"v=DMARC1; p=reject"`}},
	}
}

// drift changes shop.example's MX, so the drift alert has something to say.
func drift(f *akamai.Fake) {
	f.SetZone("shop.example", akamai.FakeZone{Records: shopZone("10 mx.mail-relay.example.net.")})
	log.Print("changed shop.example's MX (Edge DNS drift)")
}
