// Package netmap reads what the maps need from a Linux host (docs/maps.md):
// its interfaces, its WireGuard peers and a sample of its sockets, each
// attributed to the container, unit or process that holds it. The root
// helper runs it; the parsing and shrinking here take plain inputs so they
// can be tested against fixtures.
package netmap

import (
	"bufio"
	"cmp"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Caps on what one sample sends. A busy host stays well under them; past
// them the busiest lines are kept and Map.Cut is set.
const (
	MaxListen     = 500
	MaxConns      = 500
	MaxInterfaces = 64
)

// Socket is one line of /proc/net/{tcp,tcp6,udp,udp6}.
type Socket struct {
	Proto  string // tcp or udp
	Local  netip.AddrPort
	Remote netip.AddrPort
	State  int // 0x0A listening (TCP), 0x07 unconnected (UDP), 0x01 established
	Inode  uint64
}

const (
	stEstablished = 0x01
	stUDPUnconn   = 0x07
	stListen      = 0x0A
)

// ParseNetTable reads one /proc/<pid>/net/{tcp,tcp6,udp,udp6} table. Only
// listening, unconnected UDP and established sockets are returned.
func ParseNetTable(r io.Reader, proto string) ([]Socket, error) {
	var out []Socket
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first { // the header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		st, err := strconv.ParseUint(f[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("%s: state %q", proto, f[3])
		}
		switch {
		case proto == "tcp" && (st == stListen || st == stEstablished):
		case proto == "udp" && st == stUDPUnconn:
		default:
			continue
		}
		l, err1 := parseHexAddrPort(f[1])
		rm, err2 := parseHexAddrPort(f[2])
		ino, err3 := strconv.ParseUint(f[9], 10, 64)
		if err := cmp.Or(err1, err2, err3); err != nil {
			return nil, fmt.Errorf("%s: %w", proto, err)
		}
		out = append(out, Socket{Proto: proto, Local: l, Remote: rm, State: int(st), Inode: ino})
	}
	return out, sc.Err()
}

// parseHexAddrPort reads the kernel's "0100007F:1F90": the address as
// 32-bit words in host (little-endian) order, then the port in hex.
func parseHexAddrPort(s string) (netip.AddrPort, error) {
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("address %q", s)
	}
	b, err := hex.DecodeString(a)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, fmt.Errorf("address %q", s)
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("port %q", s)
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

var (
	// Docker, Podman, containerd and CRI-O scopes; a Kubernetes container
	// sits in its pod's slice.
	containerScope = regexp.MustCompile(`(?:docker|libpod|cri-containerd|crio|containerd)-([0-9a-f]{64})\.scope`)
	// cgroup v1 Docker: /docker/<id>.
	dockerV1 = regexp.MustCompile(`/docker/([0-9a-f]{64})(?:/|$)`)
)

// OwnerFromCgroup works out what holds a process from its /proc/<pid>/cgroup:
// a container, a systemd unit, or "" (the caller then uses the process name).
func OwnerFromCgroup(cg string) string {
	var path string
	for line := range strings.Lines(cg) {
		line = strings.TrimSpace(line)
		// cgroup v2 is "0::/path"; with v1 the name=systemd line names the unit.
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			path = p
			break
		}
		if _, p, ok := strings.Cut(line, ":name=systemd:"); ok {
			path = p
		}
	}
	if m := containerScope.FindStringSubmatch(path); m != nil {
		return "container:" + m[1][:12]
	}
	if m := dockerV1.FindStringSubmatch(path); m != nil {
		return "container:" + m[1][:12]
	}
	// The innermost .service: user@1000.service/app.slice/foo.service → foo.
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasSuffix(parts[i], ".service") && !strings.HasPrefix(parts[i], "user@") {
			return "unit:" + parts[i]
		}
	}
	return ""
}

// Private says whether an address is not on the Internet: RFC 1918, CGNAT,
// ULA, loopback and link-local. Connections from these keep their address.
func Private(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || cgnat.Contains(a)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Shrink turns a host's sockets into the listeners and connection groups
// a report carries. owner maps a socket inode to what holds it ("" when
// nothing does). Sockets from every network namespace may be passed
// together, as long as each namespace's are tagged with its own ns key so
// that a listener only matches connections in its own namespace. hostNS
// is the host's own namespace: listeners in any other are marked Inside
// (0: not known, none are).
func Shrink(socks []NSSocket, hostNS uint64, owner func(inode uint64) string) (listen []report.Listener, conns []report.Conn, cut bool) {
	type lkey struct {
		ns    uint64
		proto string
		port  uint16
	}
	listening := map[lkey][]netip.Addr{}
	lseen := map[report.Listener]bool{}
	for _, s := range socks {
		if s.State != stListen && s.State != stUDPUnconn {
			continue
		}
		k := lkey{s.NS, s.Proto, s.Local.Port()}
		listening[k] = append(listening[k], s.Local.Addr())
		addr := "*"
		if !s.Local.Addr().IsUnspecified() {
			addr = s.Local.Addr().String()
		}
		l := report.Listener{Owner: owner(s.Inode), Proto: s.Proto, Addr: addr, Port: int(s.Local.Port()),
			Inside: hostNS != 0 && s.NS != hostNS}
		if !lseen[l] {
			lseen[l] = true
			listen = append(listen, l)
		}
	}
	isListening := func(s NSSocket) bool {
		for _, a := range listening[lkey{s.NS, "tcp", s.Local.Port()}] {
			if a.IsUnspecified() || a == s.Local.Addr() {
				return true
			}
		}
		return false
	}

	// Both ends of a connection inside the host: one namespace to another
	// (a service in a namespace of its own, with its own public address,
	// reaching the host's mesh address), or loopback.
	type pair struct{ local, remote netip.AddrPort }
	ends := map[pair]bool{}
	for _, s := range socks {
		if s.Proto == "tcp" && s.State == stEstablished {
			ends[pair{s.Local, s.Remote}] = true
		}
	}
	counts := map[report.Conn]int{}
	for _, s := range socks {
		if s.Proto != "tcp" || s.State != stEstablished {
			continue
		}
		remote := s.Remote.Addr()
		c := report.Conn{Owner: owner(s.Inode), Inside: hostNS != 0 && s.NS != hostNS}
		if isListening(s) {
			if remote.IsLoopback() || ends[pair{s.Remote, s.Local}] {
				continue // its other end is counted going out
			}
			c.Dir, c.Port = "in", int(s.Local.Port())
			if Private(remote) {
				c.Remote = remote.String()
			}
		} else {
			c.Dir, c.Port, c.Remote = "out", int(s.Remote.Port()), remote.String()
		}
		counts[c]++
	}
	for c, n := range counts {
		c.Count = n
		conns = append(conns, c)
	}
	return capped(listen, conns)
}

// capped sorts listeners by port and connections busiest first, and cuts
// both to their caps.
func capped(listen []report.Listener, conns []report.Conn) (_ []report.Listener, _ []report.Conn, cut bool) {
	slices.SortFunc(listen, func(a, b report.Listener) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Proto, b.Proto), cmp.Compare(a.Addr, b.Addr), cmp.Compare(a.Owner, b.Owner))
	})
	slices.SortFunc(conns, func(a, b report.Conn) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Dir, b.Dir), cmp.Compare(a.Port, b.Port),
			cmp.Compare(a.Remote, b.Remote), cmp.Compare(a.Owner, b.Owner))
	})
	if len(listen) > MaxListen {
		listen, cut = listen[:MaxListen], true
	}
	if len(conns) > MaxConns {
		conns, cut = conns[:MaxConns], true
	}
	return listen, conns, cut
}

// SendEvery is how often the agent sends a map sample. It samples at every
// report and sends what it saw since the last one, merged (Merge): maps
// change slowly, and that keeps reports small.
const SendEvery = time.Minute

// Merge adds a newer sample to acc, the samples since the last send: the
// newest interfaces, gateway and WireGuard state; every listener seen; each
// connection group at its busiest. acc may be nil.
func Merge(acc, m *report.Map) *report.Map {
	if acc == nil {
		c := *m
		c.Listen, c.Conns = slices.Clone(m.Listen), slices.Clone(m.Conns)
		return &c
	}
	out := &report.Map{Interfaces: m.Interfaces, Gateway: m.Gateway, WireGuard: m.WireGuard, Error: cmp.Or(m.Error, acc.Error)}
	seen := map[report.Listener]bool{}
	for _, l := range slices.Concat(acc.Listen, m.Listen) {
		if !seen[l] {
			seen[l] = true
			out.Listen = append(out.Listen, l)
		}
	}
	busiest := map[report.Conn]int{}
	for _, c := range slices.Concat(acc.Conns, m.Conns) {
		n := c.Count
		c.Count = 0
		busiest[c] = max(busiest[c], n)
	}
	for c, n := range busiest {
		c.Count = n
		out.Conns = append(out.Conns, c)
	}
	var cut bool
	out.Listen, out.Conns, cut = capped(out.Listen, out.Conns)
	out.Cut = cut || acc.Cut || m.Cut
	return out
}

// NSSocket is a socket with the network namespace it was read in.
type NSSocket struct {
	Socket
	NS uint64
}

// ParseRoute finds the IPv4 default route's gateway in /proc/net/route.
func ParseRoute(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		a := netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]})
		if !a.IsUnspecified() {
			return a.String()
		}
	}
	return ""
}

// containerBridge names the bridges container engines and libvirt's NAT
// make: private to the host, so never what puts two hosts on one network.
var containerBridge = regexp.MustCompile(`^(docker\d+|br-[0-9a-f]{12}|cni\d*|podman\d*|virbr\d+|cali.*|flannel.*|vxlan\.calico|cilium_.*|kube-.*|weave.*)$`)

// InterfaceKind names an interface's kind from its name and its sysfs
// uevent DEVTYPE, and whether it has a device behind it.
func InterfaceKind(name, devtype string, hasDevice bool) string {
	if containerBridge.MatchString(name) {
		return "container"
	}
	switch devtype {
	case "bridge", "vlan", "bond", "wireguard":
		return devtype
	case "":
		if hasDevice {
			return "physical"
		}
		if strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "tap") {
			return "tun"
		}
		return "virtual"
	}
	return devtype
}
