package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The fake fleet's shape for the maps (docs/maps.md): sites of siteSize
// hosts, each on its own private /24. In a site, by position:
//
//	0      the site's gateway: WireGuard to every other site's gateway
//	1-2    load balancers: Internet in on :443, out to every app on :8080
//	3-29   apps: the host's containers listen on :8080 and talk to the
//	       site's databases (:5432), caches (:6379) and queue (:4222)
//	30-39  PostgreSQL (a unit)
//	40-44  Redis (a unit)
//	45-49  NATS (a unit)
const siteSize = 50

func fleetIndex(i int, name string) int {
	if n := hostIndex(name); n >= 0 {
		return n
	}
	return i
}

func siteAddr(site, pos int) string {
	return fmt.Sprintf("10.%d.%d.%d", 100+site/250, site%250, 10+pos)
}

func wgKey(site int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fleet-site-%d", site)))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// fakeMap makes host i's map sample; its site and place in it come from
// the number in its name.
func fakeMap(i int, name string, cs *report.Containers) *report.Map {
	n := fleetIndex(i, name)
	site, pos := n/siteSize, n%siteSize
	m := &report.Map{
		Interfaces: []report.Interface{
			{Name: "eth0", Kind: "physical", Addrs: []string{fmt.Sprintf("203.0.%d.%d/24", site%250, 10+pos)}, Up: true},
			{Name: "eth1", Kind: "vlan", Addrs: []string{siteAddr(site, pos) + "/24"}, Up: true},
		},
		Gateway: fmt.Sprintf("203.0.%d.1", site%250),
	}
	ssh := report.Listener{Owner: "unit:ssh.service", Proto: "tcp", Addr: "*", Port: 22}
	m.Listen = append(m.Listen, ssh)
	in := func(owner string, port, count int, from ...int) {
		if len(from) == 0 {
			m.Conns = append(m.Conns, report.Conn{Owner: owner, Dir: "in", Port: port, Count: count})
			return
		}
		for _, f := range from {
			m.Conns = append(m.Conns, report.Conn{Owner: owner, Dir: "in", Remote: siteAddr(site, f), Port: port, Count: count})
		}
	}
	out := func(owner string, port, count int, to ...int) {
		for _, t := range to {
			m.Conns = append(m.Conns, report.Conn{Owner: owner, Dir: "out", Remote: siteAddr(site, t), Port: port, Count: count})
		}
	}
	apps := func() []int {
		var a []int
		for p := 3; p < 30; p++ {
			a = append(a, p)
		}
		return a
	}
	switch {
	case pos == 0:
		m.Interfaces = append(m.Interfaces, report.Interface{Name: "wg0", Kind: "wireguard",
			Addrs: []string{fmt.Sprintf("10.250.%d.%d/16", site/250, site%250+1)}, Up: true})
		dev := report.WGDevice{Name: "wg0", PublicKey: wgKey(site), ListenPort: 51820, Peers: []report.WGPeer{}}
		for other := range fleetSites {
			if other == site {
				continue
			}
			dev.Peers = append(dev.Peers, report.WGPeer{PublicKey: wgKey(other), Endpoint: fmt.Sprintf("203.0.%d.10:51820", other%250),
				AllowedIPs: []string{fmt.Sprintf("10.%d.%d.0/24", 100+other/250, other%250)},
				Handshake:  time.Now().Add(-time.Duration(30+other%60) * time.Second).UTC(), Rx: 1 << 20, Tx: 1 << 20})
		}
		m.WireGuard = []report.WGDevice{dev}
		m.Listen = append(m.Listen, report.Listener{Proto: "udp", Addr: "*", Port: 51820})
	case pos <= 2:
		m.Listen = append(m.Listen, report.Listener{Owner: "unit:haproxy.service", Proto: "tcp", Addr: "*", Port: 443})
		in("unit:haproxy.service", 443, 40+pos*7)
		out("unit:haproxy.service", 8080, 4, apps()...)
	case pos < 30:
		owner := "process:app"
		if cs != nil && len(cs.List) > 0 {
			owner = "container:" + cs.List[0].Name
		}
		m.Listen = append(m.Listen, report.Listener{Owner: owner, Proto: "tcp", Addr: "*", Port: 8080})
		in(owner, 8080, 4, 1, 2)
		out(owner, 5432, 6, 30+pos%10)
		out(owner, 6379, 3, 40+pos%5)
		out(owner, 4222, 1, 45+pos%5)
		if fleetClusters > 0 && pos%9 == 0 { // some apps call a cluster's gateway
			m.Conns = append(m.Conns, report.Conn{Owner: owner, Dir: "out", Remote: lbAddr(site % fleetClusters), Port: 443, Count: 2})
		}
	case pos < 40:
		m.Listen = append(m.Listen, report.Listener{Owner: "unit:postgresql.service", Proto: "tcp", Addr: "*", Port: 5432})
		var from []int
		for p := 3; p < 30; p++ {
			if 30+p%10 == pos {
				from = append(from, p)
			}
		}
		in("unit:postgresql.service", 5432, 6, from...)
		if pos != 30 { // replicas stream from the primary
			out("unit:postgresql.service", 5432, 1, 30)
		}
	case pos < 45:
		m.Listen = append(m.Listen, report.Listener{Owner: "unit:redis-server.service", Proto: "tcp", Addr: "*", Port: 6379})
	default:
		m.Listen = append(m.Listen, report.Listener{Owner: "unit:nats-server.service", Proto: "tcp", Addr: "*", Port: 4222})
	}
	return m
}

// fleetSites is the number of sites, set from the run's agent count;
// fleetClusters, the number of cluster agents.
var fleetSites, fleetClusters = 1, 0
