package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A fake Cloudflare account for the demo (fleet run -cloudflare DIR): a
// dozen zones, a load-balanced pool per site whose origins are the sites'
// load balancers, and the last 15 minutes of traffic. obs-beacond reads it
// through cloudflare's stand_in (http on loopback only). Problems on
// purpose: legacy.example still uses Flexible SSL, partners.example's edge
// certificate ends in 9 days, media.example's advanced certificate is
// stuck validating, fleet-0052 fails its pool's health check from eastern
// North America, and 7% of shop.example's requests fail, most on
// checkout.shop.example.
const cloudflarePort = 58430

// cfToken is the fake's API token; not a secret, as the fake is loopback only.
const cfToken = "fleet-cloudflare-demo-token"

var cfZones = []string{"example.com", "example.net", "example.org", "shop.example", "api.example", "docs.example",
	"status.example", "corp.example", "media.example", "partners.example", "mail.example", "legacy.example"}

// cfOrigin is site s's load balancer at pos as Cloudflare knows it.
func cfOrigin(s, pos int) (name, addr string) {
	return hostName(s*siteSize + pos), fmt.Sprintf("192.0.2.%d", 10+s*10+pos)
}

func serveCloudflare(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cloudflarePort))
	if err != nil {
		log.Fatal(err)
	}
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte(cfToken+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(cfHandle), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	yaml := fmt.Sprintf("cloudflare:\n  token_file: %s\n  stand_in: http://%s\n  every: 1m\n  ssl_mode_zones: {mail.example: full}\n", tf, ln.Addr())
	p := filepath.Join(dir, "cloudflare.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet: Cloudflare at http://%s (%d zones, %d pools); %s has the cloudflare: section for obs-beacond.yaml", ln.Addr(), len(cfZones), min(fleetSites, 6), p)
}

func cfHandle(w http.ResponseWriter, r *http.Request) {
	ok := func(result any) {
		b, _ := json.Marshal(result)
		fmt.Fprintf(w, `{"success":true,"errors":[],"result":%s,"result_info":{"page":1,"total_pages":1}}`, b)
	}
	if r.URL.Path == "/status.json" {
		fmt.Fprint(w, `{"status":{"indicator":"none","description":"All Systems Operational"}}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+cfToken {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)
		return
	}
	zone := func(id string) (int, string, bool) {
		var k int
		if _, err := fmt.Sscanf(id, "z%d", &k); err != nil || k < 0 || k >= len(cfZones) {
			return 0, "", false
		}
		return k, cfZones[k], true
	}
	now := time.Now().UTC()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/zones":
		var zs []map[string]any
		for k, z := range cfZones {
			zs = append(zs, map[string]any{"id": fmt.Sprintf("z%d", k), "name": z, "account": map[string]string{"id": "acct-demo"}})
		}
		ok(zs)
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "settings" && parts[3] == "ssl":
		_, z, found := zone(parts[1])
		if !found {
			http.NotFound(w, r)
			return
		}
		mode := "strict"
		switch z {
		case "legacy.example":
			mode = "flexible"
		case "mail.example":
			mode = "full"
		}
		ok(map[string]string{"id": "ssl", "value": mode})
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "ssl" && parts[3] == "certificate_packs":
		k, z, found := zone(parts[1])
		if !found {
			http.NotFound(w, r)
			return
		}
		ends := now.Add(time.Duration(40+k*3) * 24 * time.Hour)
		if z == "partners.example" {
			ends = now.Add(9*24*time.Hour + 5*time.Hour)
		}
		packs := []map[string]any{{"type": "universal", "status": "active", "hosts": []string{z, "*." + z},
			"certificates": []map[string]string{{"expires_on": ends.Format(time.RFC3339)}}}}
		if z == "media.example" {
			packs = append(packs, map[string]any{"type": "advanced", "status": "pending_validation", "hosts": []string{"cdn." + z},
				"certificates": []map[string]string{}})
		}
		ok(packs)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "load_balancers":
		_, z, found := zone(parts[1])
		if !found {
			http.NotFound(w, r)
			return
		}
		lbs := []map[string]string{}
		switch z {
		case "shop.example":
			lbs = append(lbs, map[string]string{"name": "shop.example"}, map[string]string{"name": "checkout.shop.example"})
		case "example.com", "api.example":
			lbs = append(lbs, map[string]string{"name": "www." + z})
		}
		ok(lbs)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "dns_records":
		k, z, found := zone(parts[1])
		if !found {
			http.NotFound(w, r)
			return
		}
		_, a := cfOrigin(k%max(1, min(fleetSites, 6)), 1)
		ok([]map[string]any{
			{"id": fmt.Sprintf("r%d-1", k), "type": "A", "name": z, "content": a, "proxied": true, "ttl": 1, "modified_on": "2026-10-01T00:00:00Z"},
			{"id": fmt.Sprintf("r%d-2", k), "type": "CNAME", "name": "www." + z, "content": z, "proxied": true, "ttl": 1, "modified_on": "2026-10-01T00:00:00Z"},
			{"id": fmt.Sprintf("r%d-3", k), "type": "MX", "name": z, "content": "mx1.mail.example", "proxied": false, "ttl": 3600, "priority": 10},
			{"id": fmt.Sprintf("r%d-4", k), "type": "TXT", "name": z, "content": "v=spf1 mx -all", "proxied": false, "ttl": 3600},
		})
	case r.URL.Path == "/accounts/acct-demo/load_balancers/pools":
		var pools []map[string]any
		for s := range min(fleetSites, 6) {
			var origins []map[string]any
			for pos := 1; pos <= 2; pos++ {
				n, a := cfOrigin(s, pos)
				origins = append(origins, map[string]any{"name": n, "address": a, "enabled": true})
			}
			pools = append(pools, map[string]any{"id": fmt.Sprintf("pool-%d", s), "name": fmt.Sprintf("site-%d-web", s), "enabled": true, "healthy": true, "origins": origins})
		}
		ok(pools)
	case len(parts) == 6 && parts[2] == "load_balancers" && parts[3] == "pools" && parts[5] == "health":
		var s int
		if _, err := fmt.Sscanf(parts[4], "pool-%d", &s); err != nil {
			http.NotFound(w, r)
			return
		}
		regions := map[string]any{}
		for ri, region := range []string{"WEU", "ENAM", "WNAM", "SEAS"} {
			var origins []map[string]any
			for pos := 1; pos <= 2; pos++ {
				_, a := cfOrigin(s, pos)
				h := map[string]any{"healthy": true, "rtt": fmt.Sprintf("%dms", 20+ri*30+pos*3), "failure_reason": "No failures", "response_code": 200}
				if s == 1 && pos == 2 && region == "ENAM" {
					h = map[string]any{"healthy": false, "rtt": "0ms", "failure_reason": "Response code mismatch error", "response_code": 503}
				}
				origins = append(origins, map[string]any{a: h})
			}
			regions[region] = map[string]any{"healthy": true, "origins": origins}
		}
		ok(map[string]any{"pool_id": parts[4], "pop_health": regions})
	case r.URL.Path == "/graphql" && r.Method == http.MethodPost:
		var body struct {
			Variables struct {
				Tags []string `json:"tags"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		var zs []map[string]any
		for _, id := range body.Variables.Tags {
			k, z, found := zone(id)
			if !found {
				continue
			}
			// Requests in the last quarter hour: a busy shop, quieter
			// corners, with a little noise from minute to minute.
			base := 4000 + k*900 + int(now.Unix()/60%7)*37
			g := func(n, status int, action, host string) map[string]any {
				return map[string]any{"count": n, "dimensions": map[string]any{"edgeResponseStatus": status, "securityAction": action, "clientRequestHTTPHost": host}}
			}
			win := []map[string]any{g(base, 200, "unknown", z), g(base/20, 304, "unknown", "www."+z), g(3, 502, "unknown", z), g(base/400, 403, "block", z)}
			if z == "shop.example" {
				win = append(win, g(base*6/100, 503, "unknown", "checkout.shop.example"), g(base/100, 502, "unknown", z))
			}
			zs = append(zs, map[string]any{"zoneTag": id, "win": win, "day": []map[string]int{{"count": base / 400 * 96}}})
		}
		b, _ := json.Marshal(zs)
		fmt.Fprintf(w, `{"data":{"viewer":{"zones":%s}},"errors":null}`, b)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":7003,"message":"No route for that URI"}]}`)
	}
}
