package netmap

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func le(words ...uint32) []byte {
	b := make([]byte, 4*len(words))
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

// netPort is a port as Windows keeps it: network order in the low word.
func netPort(p uint16) uint32 { return uint32(p>>8) | uint32(p&0xff)<<8 }

func addr4(a string) uint32 {
	b := netip.MustParseAddr(a).As4()
	return binary.LittleEndian.Uint32(b[:])
}

func TestParseWinTable(t *testing.T) {
	tcp4 := append(le(3),
		append(le(winTCPListen, addr4("0.0.0.0"), netPort(443), 0, 0, 1200),
			append(le(winTCPEstablished, addr4("10.0.0.5"), netPort(443), addr4("10.0.0.9"), netPort(51000), 1200),
				le(8 /* TIME_WAIT */, addr4("10.0.0.5"), netPort(50000), addr4("10.0.0.7"), netPort(5432), 0)...)...)...)
	got, err := ParseWinTable(tcp4, "tcp", false)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	if got[0].State != stListen || got[0].Local.Port() != 443 || !got[0].Local.Addr().IsUnspecified() || got[0].Inode != 1200 {
		t.Fatalf("listener %+v", got[0])
	}
	if got[1].State != stEstablished || got[1].Remote != netip.MustParseAddrPort("10.0.0.9:51000") {
		t.Fatalf("conn %+v", got[1])
	}

	// IPv6 TCP: a v4-mapped connection comes out as v4.
	row := make([]byte, 56)
	m := netip.MustParseAddr("::ffff:10.0.0.5").As16()
	copy(row[0:16], m[:])
	binary.LittleEndian.PutUint32(row[20:], netPort(8080))
	r := netip.MustParseAddr("fd00::7").As16()
	copy(row[24:40], r[:])
	binary.LittleEndian.PutUint32(row[44:], netPort(60000))
	binary.LittleEndian.PutUint32(row[48:], winTCPEstablished)
	binary.LittleEndian.PutUint32(row[52:], 88)
	got, err = ParseWinTable(append(le(1), row...), "tcp", true)
	if err != nil || len(got) != 1 || got[0].Local != netip.MustParseAddrPort("10.0.0.5:8080") || got[0].Remote.Addr() != netip.MustParseAddr("fd00::7") || got[0].Inode != 88 {
		t.Fatalf("%v %+v", err, got)
	}

	udp4 := append(le(1), le(addr4("127.0.0.1"), netPort(53), 900)...)
	got, err = ParseWinTable(udp4, "udp", false)
	if err != nil || len(got) != 1 || got[0].State != stUDPUnconn || got[0].Local != netip.MustParseAddrPort("127.0.0.1:53") {
		t.Fatalf("%v %+v", err, got)
	}
	udp6 := append(le(1), make([]byte, 28)...)
	binary.LittleEndian.PutUint32(udp6[4+20:], netPort(5353))
	if got, err = ParseWinTable(udp6, "udp", true); err != nil || len(got) != 1 || got[0].Local.Port() != 5353 {
		t.Fatalf("%v %+v", err, got)
	}

	if _, err := ParseWinTable(le(2, 1, 2, 3), "tcp", false); err == nil {
		t.Fatal("a short table parsed")
	}
}

func TestWinOwner(t *testing.T) {
	svcs := map[uint32]string{1200: "W3SVC"}
	exe := map[uint32]string{1200: "svchost.exe", 900: "sqlservr.exe", 4: "System"}
	for pid, want := range map[uint32]string{1200: "unit:W3SVC.service", 900: "process:sqlservr", 4: "process:System", 0: "", 77: ""} {
		if got := WinOwner(pid, svcs, exe); got != want {
			t.Errorf("%d: %q, want %q", pid, got, want)
		}
	}
}
