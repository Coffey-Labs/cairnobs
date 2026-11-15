package netmap

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

const tcpTable = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 100 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 101 1 0000000000000000 100 0 0 10 0
   2: 0200000A:0016 0300000A:D431 01 00000000:00000000 02:00000000 00000000     0        0 102 4 0000000000000000 20 4 30 10 -1
   3: 0200000A:0016 08080808:C350 01 00000000:00000000 02:00000000 00000000     0        0 103 4 0000000000000000 20 4 30 10 -1
   4: 0200000A:0016 04040808:C351 01 00000000:00000000 02:00000000 00000000     0        0 104 4 0000000000000000 20 4 30 10 -1
   5: 0200000A:9C40 0400000A:1538 01 00000000:00000000 02:00000000 00000000  1000        0 105 4 0000000000000000 20 4 30 10 -1
   6: 0200000A:9C41 0400000A:1538 01 00000000:00000000 02:00000000 00000000  1000        0 106 4 0000000000000000 20 4 30 10 -1
   7: 0100007F:9C42 0100007F:1538 01 00000000:00000000 02:00000000 00000000  1000        0 107 4 0000000000000000 20 4 30 10 -1
   8: 0100007F:1538 0100007F:9C42 01 00000000:00000000 02:00000000 00000000  1000        0 108 4 0000000000000000 20 4 30 10 -1
   9: 0200000A:9C43 0400000A:1538 06 00000000:00000000 02:00000000 00000000  1000        0 0 4 0000000000000000 20 4 30 10 -1
`

const tcp6Table = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 200 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000200000A:01BB 0000000000000000FFFF00000500000A:E000 01 00000000:00000000 00:00000000 00000000     0        0 201 1 0000000000000000 100 0 0 10 0
`

func TestParseNetTable(t *testing.T) {
	socks, err := ParseNetTable(strings.NewReader(tcpTable), "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 9 { // the TIME_WAIT line (06) is left out
		t.Fatalf("got %d sockets, want 9", len(socks))
	}
	if s := socks[2]; s.Local.String() != "10.0.0.2:22" || s.Remote.String() != "10.0.0.3:54321" || s.State != stEstablished || s.Inode != 102 {
		t.Errorf("line 2 = %+v", s)
	}
	six, err := ParseNetTable(strings.NewReader(tcp6Table), "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if six[0].Local.String() != "[::]:443" || six[1].Local.String() != "10.0.0.2:443" || six[1].Remote.Addr().String() != "10.0.0.5" {
		t.Errorf("tcp6 = %v %v %v", six[0].Local, six[1].Local, six[1].Remote)
	}
}

func TestOwnerFromCgroup(t *testing.T) {
	id := strings.Repeat("ab", 32)
	for _, c := range []struct{ cg, want string }{
		{"0::/system.slice/nginx.service\n", "unit:nginx.service"},
		{"0::/system.slice/docker-" + id + ".scope\n", "container:abababababab"},
		{"0::/machine.slice/libpod-" + id + ".scope/container\n", "container:abababababab"},
		{"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-" + id + ".scope\n", "container:abababababab"},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/syncthing.service\n", "unit:syncthing.service"},
		{"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
		{"12:pids:/docker/" + id + "\n1:name=systemd:/docker/" + id + "\n", "container:abababababab"},
		{"1:name=systemd:/system.slice/patroni.service\n", "unit:patroni.service"},
	} {
		if got := OwnerFromCgroup(c.cg); got != c.want {
			t.Errorf("%q: got %q, want %q", c.cg, got, c.want)
		}
	}
}

func TestShrink(t *testing.T) {
	var socks []NSSocket
	for _, tbl := range []string{tcpTable, tcp6Table} {
		list, err := ParseNetTable(strings.NewReader(tbl), "tcp")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			socks = append(socks, NSSocket{Socket: s, NS: 1})
		}
	}
	// A second namespace (a container) that also listens on 22: its
	// listener mustn't turn the host's outgoing connections into incoming.
	socks = append(socks, NSSocket{NS: 2, Socket: Socket{Proto: "tcp", State: stListen, Inode: 300,
		Local: netip.MustParseAddrPort("172.17.0.2:5432")}})
	owners := map[uint64]string{100: "unit:ssh.service", 101: "process:sidecar", 102: "unit:ssh.service", 103: "unit:ssh.service",
		104: "unit:ssh.service", 105: "container:aaaaaaaaaaaa", 106: "container:aaaaaaaaaaaa", 107: "unit:app.service",
		108: "process:sidecar", 200: "unit:nginx.service", 201: "unit:nginx.service", 300: "container:bbbbbbbbbbbb"}
	listen, conns, cut := Shrink(socks, 1, func(i uint64) string { return owners[i] })
	if cut {
		t.Error("cut, want whole")
	}
	wantL := []report.Listener{
		{Owner: "unit:ssh.service", Proto: "tcp", Addr: "*", Port: 22},
		{Owner: "unit:nginx.service", Proto: "tcp", Addr: "*", Port: 443},
		{Owner: "process:sidecar", Proto: "tcp", Addr: "127.0.0.1", Port: 5432},
		{Owner: "container:bbbbbbbbbbbb", Proto: "tcp", Addr: "172.17.0.2", Port: 5432, Inside: true},
	}
	if !slices.Equal(listen, wantL) {
		t.Errorf("listen =\n%v\nwant\n%v", listen, wantL)
	}
	wantC := []report.Conn{
		// Two public clients on :22, counted, with no address.
		{Owner: "unit:ssh.service", Dir: "in", Port: 22, Count: 2},
		// Two connections out to 10.0.0.4:5432 from one container.
		{Owner: "container:aaaaaaaaaaaa", Dir: "out", Remote: "10.0.0.4", Port: 5432, Count: 2},
		{Owner: "unit:ssh.service", Dir: "in", Remote: "10.0.0.3", Port: 22, Count: 1},
		{Owner: "unit:nginx.service", Dir: "in", Remote: "10.0.0.5", Port: 443, Count: 1},
		// Loopback: only the end that connected.
		{Owner: "unit:app.service", Dir: "out", Remote: "127.0.0.1", Port: 5432, Count: 1},
	}
	if !slices.Equal(conns, wantC) {
		t.Errorf("conns =\n%v\nwant\n%v", conns, wantC)
	}
	for _, c := range conns {
		if c.Remote == "8.8.8.8" || c.Remote == "8.8.4.4" {
			t.Errorf("an Internet client's address was kept: %+v", c)
		}
	}
}

func TestShrinkCaps(t *testing.T) {
	var socks []NSSocket
	for i := range MaxConns + 50 {
		socks = append(socks, NSSocket{NS: 1, Socket: Socket{Proto: "tcp", State: stEstablished, Inode: uint64(i + 1),
			Local:  netip.MustParseAddrPort("10.0.0.2:40000"),
			Remote: netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}), 443)}})
	}
	_, conns, cut := Shrink(socks, 1, func(uint64) string { return "unit:x.service" })
	if !cut || len(conns) != MaxConns {
		t.Errorf("cut %v, %d conns; want cut at %d", cut, len(conns), MaxConns)
	}
}

func TestParseRoute(t *testing.T) {
	const route = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0000000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	if g := ParseRoute(strings.NewReader(route)); g != "10.0.0.1" {
		t.Errorf("gateway %q, want 10.0.0.1", g)
	}
}

func TestInterfaceKind(t *testing.T) {
	for _, c := range []struct {
		name, devtype string
		dev           bool
		want          string
	}{
		{"eno1", "", true, "physical"},
		{"docker0", "bridge", false, "container"},
		{"br-c71d59ac4344", "bridge", false, "container"},
		{"br0", "bridge", false, "bridge"},
		{"wg-bench", "wireguard", false, "wireguard"},
		{"enp3s0.20", "vlan", false, "vlan"},
		{"tun0", "", false, "tun"},
		{"virbr0", "bridge", false, "container"},
	} {
		if got := InterfaceKind(c.name, c.devtype, c.dev); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// nested builds a nested netlink attribute.
func nested(typ uint16, parts ...[]byte) []byte {
	return nlAttrT(typ|nlaFNested, slices.Concat(parts...))
}

func nlAttrT(typ uint16, data []byte) []byte {
	l := 4 + len(data)
	b := make([]byte, (l+3)&^3)
	binary.NativeEndian.PutUint16(b[0:2], uint16(l))
	binary.NativeEndian.PutUint16(b[2:4], typ)
	copy(b[4:], data)
	return b
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.NativeEndian.PutUint16(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.NativeEndian.PutUint64(b, v); return b }

func TestParseWGDevice(t *testing.T) {
	pub := make([]byte, 32)
	priv := make([]byte, 32)
	peer := make([]byte, 32)
	psk := make([]byte, 32)
	for i := range 32 {
		pub[i], priv[i], peer[i], psk[i] = 1, 0xEE, 2, 0xDD
	}
	ep := make([]byte, 16) // sockaddr_in 10.0.0.3:51820
	binary.NativeEndian.PutUint16(ep[0:2], afInet)
	binary.BigEndian.PutUint16(ep[2:4], 51820)
	copy(ep[4:8], []byte{10, 0, 0, 3})
	hs := slices.Concat(u64(1_763_000_000), u64(5))
	msg := slices.Concat(
		nlAttrT(wgdeviceAIfname, append([]byte("wg0"), 0)),
		nlAttrT(wgdeviceAPrivateKey, priv),
		nlAttrT(wgdeviceAPublicKey, pub),
		nlAttrT(wgdeviceAListenPort, u16(51820)),
		nested(wgdeviceAPeers,
			nested(0,
				nlAttrT(wgpeerAPublicKey, peer),
				nlAttrT(wgpeerAPresharedKey, psk),
				nlAttrT(wgpeerAEndpoint, ep),
				nlAttrT(wgpeerALastHandshakeTime, hs),
				nlAttrT(wgpeerARxBytes, u64(1000)),
				nlAttrT(wgpeerATxBytes, u64(2000)),
				nested(wgpeerAAllowedIPs,
					nested(0, nlAttrT(wgallowedipAFamily, u16(afInet)), nlAttrT(wgallowedipAIPAddr, []byte{10, 20, 0, 3}), nlAttrT(wgallowedipACidrMask, []byte{32})),
				),
			),
		),
	)
	d := parseWGDevice(msg)
	if d.Name != "wg0" || d.PublicKey != "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" || d.ListenPort != 51820 || len(d.Peers) != 1 {
		t.Fatalf("device = %+v", d)
	}
	p := d.Peers[0]
	if p.PublicKey != "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=" || p.Endpoint != "10.0.0.3:51820" || p.Rx != 1000 || p.Tx != 2000 ||
		!p.Handshake.Equal(time.Unix(1_763_000_000, 5)) || !slices.Equal(p.AllowedIPs, []string{"10.20.0.3/32"}) {
		t.Errorf("peer = %+v", p)
	}
	// The private and preshared keys were wiped where they lay.
	if slices.Contains(msg, 0xEE) || slices.Contains(msg, 0xDD) {
		t.Error("a secret key is still in the kernel's answer")
	}
	merged := mergeDevices([]report.WGDevice{d, {Name: "wg0", Peers: []report.WGPeer{{PublicKey: "x"}}}, {Name: "wg1"}})
	if len(merged) != 2 || len(merged[0].Peers) != 2 || merged[1].Peers == nil {
		t.Errorf("merged = %+v", merged)
	}
}

func TestMerge(t *testing.T) {
	a := &report.Map{Gateway: "10.0.0.1", Listen: []report.Listener{{Owner: "unit:a.service", Proto: "tcp", Addr: "*", Port: 80}},
		Conns: []report.Conn{{Owner: "unit:a.service", Dir: "out", Remote: "10.0.0.9", Port: 5432, Count: 3}}}
	b := &report.Map{Gateway: "10.0.0.254", Error: "sockets: x",
		Listen: []report.Listener{{Owner: "unit:a.service", Proto: "tcp", Addr: "*", Port: 80}, {Owner: "unit:b.service", Proto: "tcp", Addr: "*", Port: 81}},
		Conns: []report.Conn{{Owner: "unit:a.service", Dir: "out", Remote: "10.0.0.9", Port: 5432, Count: 1},
			{Owner: "unit:b.service", Dir: "in", Port: 81, Count: 7}}}
	m := Merge(Merge(nil, a), b)
	if m.Gateway != "10.0.0.254" || m.Error != "sockets: x" || len(m.Listen) != 2 {
		t.Errorf("merged = %+v", m)
	}
	want := []report.Conn{{Owner: "unit:b.service", Dir: "in", Port: 81, Count: 7}, {Owner: "unit:a.service", Dir: "out", Remote: "10.0.0.9", Port: 5432, Count: 3}}
	if !slices.Equal(m.Conns, want) {
		t.Errorf("conns = %v, want %v", m.Conns, want)
	}
	if len(a.Conns) != 1 || a.Conns[0].Count != 3 {
		t.Error("Merge changed its input")
	}
}

// inbuxa in a namespace of its own, with its own public address, reaching
// HAProxy on the host's mesh address (ours on host2, 2026-11-14): the host
// sees both ends. Counted once, going out, never as the Internet coming in.
func TestShrinkAcrossNamespaces(t *testing.T) {
	mail, mesh := netip.MustParseAddrPort("95.216.2.52:49890"), netip.MustParseAddrPort("10.77.0.2:5000")
	socks := []NSSocket{
		{NS: 1, Socket: Socket{Proto: "tcp", State: stListen, Inode: 1, Local: netip.MustParseAddrPort("0.0.0.0:5000")}},
		{NS: 1, Socket: Socket{Proto: "tcp", State: stEstablished, Inode: 2, Local: mesh, Remote: mail}},
		{NS: 3, Socket: Socket{Proto: "tcp", State: stEstablished, Inode: 3, Local: mail, Remote: mesh}},
		// A real client from the Internet on the same port stays.
		{NS: 1, Socket: Socket{Proto: "tcp", State: stEstablished, Inode: 4, Local: mesh, Remote: netip.MustParseAddrPort("8.8.8.8:5555")}},
	}
	owners := map[uint64]string{1: "unit:haproxy.service", 2: "unit:haproxy.service", 3: "unit:inbuxa.service", 4: "unit:haproxy.service"}
	_, conns, _ := Shrink(socks, 1, func(i uint64) string { return owners[i] })
	want := []report.Conn{
		{Owner: "unit:haproxy.service", Dir: "in", Port: 5000, Count: 1},
		{Owner: "unit:inbuxa.service", Dir: "out", Remote: "10.77.0.2", Port: 5000, Count: 1, Inside: true},
	}
	slices.SortFunc(conns, func(a, b report.Conn) int { return strings.Compare(a.Owner, b.Owner) })
	if !slices.Equal(conns, want) {
		t.Errorf("conns =\n%v\nwant\n%v", conns, want)
	}
}
