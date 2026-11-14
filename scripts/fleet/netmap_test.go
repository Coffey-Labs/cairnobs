package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Every connection the fake fleet makes out lands on a listener that the
// host at that address has, and samples stay small.
func TestFakeMapLinesUp(t *testing.T) {
	const n = 1000
	fleetSites = n / siteSize
	type ep struct {
		addr string
		port int
	}
	listening := map[ep]bool{}
	var outs []ep
	most, total := 0, 0
	var plainOne []byte
	for i := range n {
		name := hostName(i)
		h := newHost(i, name, 10, 15*time.Second)
		plain, _ := json.Marshal(h.r)
		plainOne = plain
		h.r.Map = fakeMap(i, name, h.r.Containers)
		with, _ := json.Marshal(h.r)
		grew := len(with) - len(plain)
		most, total = max(most, grew), total+grew
		addr := strings.TrimSuffix(h.r.Map.Interfaces[1].Addrs[0], "/24")
		for _, l := range h.r.Map.Listen {
			listening[ep{addr, l.Port}] = true
		}
		for _, c := range h.r.Map.Conns {
			if c.Dir == "out" {
				outs = append(outs, ep{c.Remote, c.Port})
			}
		}
	}
	for _, o := range outs {
		if !listening[o] {
			t.Errorf("a connection out to %s:%d, where nothing listens", o.addr, o.port)
		}
	}
	t.Logf("map sample: %d bytes on average, %d at most; reports without it %d bytes", total/n, most, len(plainOne))
}
