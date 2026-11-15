package netmap

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// Windows' socket tables, as GetExtendedTcpTable and GetExtendedUdpTable
// return them with their owning process (TCP_TABLE_OWNER_PID_ALL,
// UDP_TABLE_OWNER_PID): a count, then fixed-size rows of 32-bit words in
// the machine's order (little-endian), with addresses and ports in network
// order. Parsed here, apart from the calls (sample_windows.go), so tests
// run anywhere. The process ID stands where Linux has a socket's inode.

const (
	winTCPListen      = 2 // MIB_TCP_STATE_LISTEN
	winTCPEstablished = 5 // MIB_TCP_STATE_ESTAB
)

var errShortTable = errors.New("short table")

func winPort(w uint32) uint16 { return uint16(w&0xff)<<8 | uint16(w>>8&0xff) }

func winState(proto string, st uint32) (int, bool) {
	if proto == "udp" {
		return stUDPUnconn, true
	}
	switch st {
	case winTCPListen:
		return stListen, true
	case winTCPEstablished:
		return stEstablished, true
	}
	return 0, false
}

// ParseWinTable reads one table: proto is tcp or udp, v6 says which
// address family it holds.
func ParseWinTable(b []byte, proto string, v6 bool) ([]Socket, error) {
	if len(b) < 4 {
		return nil, errShortTable
	}
	n := int(binary.LittleEndian.Uint32(b))
	var size int
	switch {
	case proto == "tcp" && !v6:
		size = 24 // state, local addr, local port, remote addr, remote port, pid
	case proto == "tcp":
		size = 56 // local addr[16], scope, port, remote addr[16], scope, port, state, pid
	case !v6:
		size = 12 // local addr, local port, pid
	default:
		size = 28 // local addr[16], scope, port, pid
	}
	rows := b[4:]
	if n < 0 || len(rows) < n*size {
		return nil, errShortTable
	}
	u32 := func(r []byte, off int) uint32 { return binary.LittleEndian.Uint32(r[off:]) }
	var out []Socket
	for i := range n {
		r := rows[i*size : (i+1)*size]
		var s Socket
		var st, pid uint32
		switch size {
		case 24:
			st, pid = u32(r, 0), u32(r, 20)
			s.Local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[4:8])), winPort(u32(r, 8)))
			s.Remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[12:16])), winPort(u32(r, 16)))
		case 56:
			st, pid = u32(r, 48), u32(r, 52)
			s.Local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])).Unmap(), winPort(u32(r, 20)))
			s.Remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[24:40])).Unmap(), winPort(u32(r, 44)))
		case 12:
			pid = u32(r, 8)
			s.Local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[0:4])), winPort(u32(r, 4)))
		case 28:
			pid = u32(r, 24)
			s.Local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])).Unmap(), winPort(u32(r, 20)))
		}
		state, ok := winState(proto, st)
		if !ok {
			continue
		}
		s.Proto, s.State, s.Inode = proto, state, uint64(pid)
		out = append(out, s)
	}
	return out, nil
}

// WinOwner names what holds a process on Windows: the service it runs
// (as a unit, "unit:W32Time.service", as obs-beacond names Windows
// services), else the program's name without .exe; "" for the idle
// process.
func WinOwner(pid uint32, services map[uint32]string, exe map[uint32]string) string {
	if pid == 0 {
		return ""
	}
	if s, ok := services[pid]; ok {
		return "unit:" + s + ".service"
	}
	if e, ok := exe[pid]; ok && e != "" {
		if len(e) > 4 && (e[len(e)-4:] == ".exe" || e[len(e)-4:] == ".EXE") {
			e = e[:len(e)-4]
		}
		return "process:" + e
	}
	return ""
}
