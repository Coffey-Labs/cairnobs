package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cairnobs/cairnobs/internal/akamai"
)

// The fleet's fake Akamai (docs/akamai.md, step 5): an enterprise
// account on loopback, every signature checked. GTM's datacenters are
// the first sites and its servers their load balancers' public
// addresses (netmap.go, positions 1 and 2), so a failing one is a fleet
// host. Problems on purpose, the ones marked * coming and going
// (scenario.go):
//
//	GTM        * site 2's first load balancer failing for api.shop;
//	           * the legacy property with no live datacenter (story 3)
//	CPS        180 enrollments: a third-party renewal waiting on a CSR,
//	           ending in 15 days
//	Property   320 properties: a failed production activation;
//	Manager    * one rolling out for longer than it should
//	Edge DNS   40 zones (one of 5,000 records); one changes an MX 20
//	           minutes after the start
//	Traffic    320 CP codes; * the legacy site's at 30% 5xx while its
//	           datacenter is down (story 3)
const akamaiPort = 58410

func serveAkamai(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", akamaiPort))
	if err != nil {
		log.Fatal(err)
	}
	c := akamai.Creds{Host: ln.Addr().String(), ClientToken: "akab-fleet-client", ClientSecret: "ZmxlZXQtc2VjcmV0", AccessToken: "akab-fleet-access"}
	edgerc := filepath.Join(dir, ".edgerc")
	body := fmt.Sprintf("[default]\nhost = http://%s\nclient_token = %s\nclient_secret = %s\naccess_token = %s\n", c.Host, c.ClientToken, c.ClientSecret, c.AccessToken)
	if err := os.WriteFile(edgerc, []byte(body), 0o600); err != nil {
		log.Fatal(err)
	}
	f := &akamai.Fake{Creds: c}
	start := time.Now()
	n := fleetAkamai(f)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				akamaiStory(f, now)
			}
		}
	}()
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	time.AfterFunc(20*time.Minute, func() {
		f.SetZone("zone07.example", akamai.FakeZone{Records: fleetZone(7, "10 mx.relay.example.net.")})
		log.Print("fleet: Akamai Edge DNS: zone07.example's MX changed")
	})
	yaml := "akamai:\n  edgerc_file: " + edgerc + "\n"
	p := filepath.Join(dir, "akamai.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet: Akamai at http://%s (%s, made in %s); %s has the akamai: section for obs-beacond.yaml", c.Host, n, time.Since(start).Round(time.Millisecond), p)
}

// fleetAkamai fills the fake and says what it has.
func fleetAkamai(f *akamai.Fake) string {
	day := 24 * time.Hour
	now := time.Now()
	// GTM: datacenters are sites 0-5; each property's targets are three
	// of them, servers their load balancers' eth0 addresses.
	sites := min(fleetSites, 6)
	dcs := map[int]string{}
	cities := []string{"Ashburn", "Frankfurt", "Singapore", "Dallas", "London", "Tokyo"}
	for s := range sites {
		dcs[3100+s] = cities[s]
	}
	lb := func(site, pos int) string { return fmt.Sprintf("203.0.%d.%d", site%250, 10+pos) }
	props := 0
	for d, name := range []string{"shop", "api", "media", "corp", "eu", "apac", "status", "auth"} {
		var ps []akamai.FakeProperty
		for p := range 6 + d%3*3 {
			targets := map[int]map[string]bool{}
			for k := range min(3, sites) {
				s := (p + d + k) % sites
				targets[3100+s] = map[string]bool{lb(s, 1): true, lb(s, 2): true}
			}
			ps = append(ps, akamai.FakeProperty{Name: fmt.Sprintf("p%02d", p), Type: "performance", Targets: targets})
		}
		switch name {
		case "shop":
			ps[0].Name = "api"
		case "corp":
			ps = append(ps, akamai.FakeProperty{Name: "legacy", Type: "failover", Targets: map[int]map[string]bool{3100: {legacyServer: true}}})
		}
		props += len(ps)
		f.SetDomain(name+".example.akadns.net", akamai.FakeDomain{Type: "full", Datacenters: dcs, Properties: ps})
	}
	// CPS: ECDSA keys (fast to make); every tenth has an RSA one too.
	for i := range 180 {
		cn := fmt.Sprintf("app%03d.example.com", i)
		left := time.Duration(25+(i*37)%300) * day
		if i == 47 {
			left = 15 * day
		}
		e := akamai.FakeEnrollment{CN: cn, SANs: []string{cn, "www." + cn}, Validation: "dv", Contract: "1-FLEET", Waiting: "-"}
		issuer := "Let's Encrypt E7"
		if i%6 == 0 {
			e.Validation, issuer = "ov", "DigiCert Global G2 TLS RSA SHA256 2020 CA1"
		}
		if i == 47 {
			e.Validation, e.Waiting, issuer = "third-party", "third-party-csr", "Sectigo RSA Domain Validation Secure Server CA"
		}
		e.Deployed = []string{akamai.FakePEM(issuer, e.SANs, true, now.Add(left))}
		if i%10 == 0 {
			e.Deployed = append(e.Deployed, akamai.FakePEM(issuer, e.SANs, false, now.Add(left)))
		}
		f.SetEnrollment(200000+i, e)
	}
	// Property Manager: six groups over two contracts.
	groups := []string{"Shop", "Media", "Corporate", "APIs", "Europe", "Asia"}
	for i := range 320 {
		v := 3 + (i*7)%60
		p := akamai.FakePMProperty{Name: fmt.Sprintf("site%03d.example.com", i), Group: groups[i%len(groups)], Contract: []string{"1-FLEET", "2-FLEET"}[i%2],
			Latest: v, Staging: v, Production: v, Activations: []akamai.FakeActivation{
				{Version: v, Network: "STAGING", Status: "ACTIVE", Submitted: now.Add(-time.Duration(30+i%500) * time.Hour)},
				{Version: v, Network: "PRODUCTION", Status: "ACTIVE", Submitted: now.Add(-time.Duration(29+i%500) * time.Hour)}}}
		// v+1: on staging first, then production.
		staged := akamai.FakeActivation{Version: v + 1, Network: "STAGING", Status: "ACTIVE", Submitted: now.Add(-5 * time.Hour)}
		switch i {
		case 4:
			p.Latest, p.Staging = v+1, v+1
			p.Activations = append(p.Activations, staged,
				akamai.FakeActivation{Version: v + 1, Network: "PRODUCTION", Status: "FAILED", Submitted: now.Add(-3 * time.Hour), Note: "Origin moved to the new load balancers"})
		}
		f.SetPMProperty(fmt.Sprintf("prp_9%05d", i), p)
		if i == slowActivation {
			slowBase = p
		}
	}
	// Edge DNS.
	records := 0
	for z := range 40 {
		recs := fleetZone(z, "10 mx1.example.com.")
		if z == 0 {
			for h := range 5000 {
				recs = append(recs, akamai.RecordSet{Name: fmt.Sprintf("host%04d.zone00.example", h), Type: "A", TTL: 300, RData: []string{fmt.Sprintf("10.%d.%d.%d", 100+h/62500, h/250%250, h%250+1)}})
			}
		}
		records += len(recs)
		f.SetZone(fmt.Sprintf("zone%02d.example", z), akamai.FakeZone{Records: recs})
	}
	// Traffic: a long tail, about one request in 2,000 failing.
	for i := range 320 {
		hits := int64(2_000_000 / (i + 1))
		f.SetTraffic(int64(400000+i), hits, hits/2000)
	}
	akamaiStory(f, now)
	return fmt.Sprintf("%d GTM properties over %d datacenters, 180 CPS enrollments, 320 properties, 40 DNS zones with %d record sets, 320 CP codes", props, sites, records)
}

const (
	legacyServer   = "198.51.100.50" // the legacy property's one datacenter's server
	legacyCPCode   = 400019          // the legacy site's traffic
	slowActivation = 15              // the property whose production activation drags
)

// slowBase is the slow property as fleetAkamai made it, before its new
// version.
var slowBase akamai.FakePMProperty

// akamaiStory puts the problems that come and go in their state now
// (scenario.go): story 3, the legacy datacenter; a load balancer that
// fails GTM's liveness test now and then; an activation that drags.
func akamaiStory(f *akamai.Fake, now time.Time) {
	legacyDown := spellLegacyDC.active(now)
	f.SetAlive("corp.example.akadns.net", "legacy", 3100, legacyServer, !legacyDown)
	hits := int64(2_000_000 / (legacyCPCode - 400000 + 1))
	errs := hits / 2000
	if legacyDown {
		errs = hits * 30 / 100
	}
	f.SetTraffic(legacyCPCode, hits, errs)
	if sites := min(fleetSites, 6); sites > 2 {
		f.SetAlive("shop.example.akadns.net", "api", 3100+2, fmt.Sprintf("203.0.%d.%d", 2, 11), !spellLBFlaps.active(now))
	}
	// Its new version goes to production at the start of each spell and
	// is active an hour and twenty minutes later.
	p := slowBase
	p.Activations = append([]akamai.FakeActivation{}, p.Activations...)
	v := p.Latest + 1
	b := spellActivation.began(now)
	p.Latest, p.Staging = v, v
	p.Activations = append(p.Activations, akamai.FakeActivation{Version: v, Network: "STAGING", Status: "ACTIVE", Submitted: b.Add(-2 * time.Hour)})
	if spellActivation.active(now) {
		p.Activations = append(p.Activations, akamai.FakeActivation{Version: v, Network: "PRODUCTION", Status: "PENDING", Submitted: b})
	} else {
		p.Production = v
		p.Activations = append(p.Activations, akamai.FakeActivation{Version: v, Network: "PRODUCTION", Status: "ACTIVE", Submitted: b})
	}
	f.SetPMProperty(fmt.Sprintf("prp_9%05d", slowActivation), p)
}

// fleetZone is zone z's ordinary records, with the given MX.
func fleetZone(z int, mx string) []akamai.RecordSet {
	name := fmt.Sprintf("zone%02d.example", z)
	recs := []akamai.RecordSet{
		{Name: name, Type: "MX", TTL: 3600, RData: []string{mx}},
		{Name: name, Type: "TXT", TTL: 3600, RData: []string{`"v=spf1 mx -all"`}},
		{Name: "_dmarc." + name, Type: "TXT", TTL: 3600, RData: []string{`"v=DMARC1; p=reject"`}},
	}
	for h := range 20 + z*5 {
		recs = append(recs, akamai.RecordSet{Name: fmt.Sprintf("www%d.%s", h, name), Type: "CNAME", TTL: 300, RData: []string{fmt.Sprintf("www%d.%s.edgekey.net.", h, name)}})
	}
	return recs
}
