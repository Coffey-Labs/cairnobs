package netmap

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// WireGuard's generic netlink family (linux/wireguard.h). Only the device
// dump is used. The kernel's answer carries the private and preshared keys
// too; they're skipped while parsing and the buffers wiped.
const (
	wgCmdGetDevice = 0
	wgGenlVersion  = 1

	wgdeviceAIfname     = 2
	wgdeviceAPrivateKey = 3
	wgdeviceAPublicKey  = 4
	wgdeviceAListenPort = 6
	wgdeviceAPeers      = 8

	wgpeerAPublicKey         = 1
	wgpeerAPresharedKey      = 2
	wgpeerAEndpoint          = 4
	wgpeerALastHandshakeTime = 6
	wgpeerARxBytes           = 7
	wgpeerATxBytes           = 8
	wgpeerAAllowedIPs        = 9

	wgallowedipAFamily   = 1
	wgallowedipAIPAddr   = 2
	wgallowedipACidrMask = 3

	nlaFNested    = 0x8000
	nlaFNetByteor = 0x4000
	nlaTypeMask   = ^uint16(nlaFNested | nlaFNetByteor)

	afInet  = 2
	afInet6 = 10
)

// attr is one netlink attribute.
type attr struct {
	typ  uint16
	data []byte
}

// attrs splits a run of netlink attributes. Malformed tails are dropped.
func attrs(b []byte) []attr {
	var out []attr
	for len(b) >= 4 {
		l := int(binary.NativeEndian.Uint16(b[0:2]))
		t := binary.NativeEndian.Uint16(b[2:4]) & nlaTypeMask
		if l < 4 || l > len(b) {
			break
		}
		out = append(out, attr{t, b[4:l]})
		n := (l + 3) &^ 3
		if n > len(b) {
			n = len(b)
		}
		b = b[n:]
	}
	return out
}

func wipe(b []byte) { clear(b) }

func key(b []byte) string {
	if len(b) != 32 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// parseWGDevice reads one WG_CMD_GET_DEVICE answer's attributes (after the
// genetlink header). A large device comes in several messages, each with
// the name and more peers; the caller merges them by name.
func parseWGDevice(b []byte) report.WGDevice {
	var d report.WGDevice
	for _, a := range attrs(b) {
		switch a.typ {
		case wgdeviceAIfname:
			d.Name = cstring(a.data)
		case wgdeviceAPublicKey:
			d.PublicKey = key(a.data)
		case wgdeviceAPrivateKey:
			wipe(a.data)
		case wgdeviceAListenPort:
			if len(a.data) >= 2 {
				d.ListenPort = int(binary.NativeEndian.Uint16(a.data))
			}
		case wgdeviceAPeers:
			for _, p := range attrs(a.data) {
				d.Peers = append(d.Peers, parseWGPeer(p.data))
			}
		}
	}
	return d
}

func parseWGPeer(b []byte) report.WGPeer {
	var p report.WGPeer
	for _, a := range attrs(b) {
		switch a.typ {
		case wgpeerAPublicKey:
			p.PublicKey = key(a.data)
		case wgpeerAPresharedKey:
			wipe(a.data)
		case wgpeerAEndpoint:
			if ap, ok := sockaddr(a.data); ok {
				p.Endpoint = ap.String()
			}
		case wgpeerALastHandshakeTime:
			if len(a.data) >= 16 { // struct __kernel_timespec
				s := int64(binary.NativeEndian.Uint64(a.data[0:8]))
				ns := int64(binary.NativeEndian.Uint64(a.data[8:16]))
				if s != 0 || ns != 0 {
					p.Handshake = time.Unix(s, ns).UTC()
				}
			}
		case wgpeerARxBytes:
			if len(a.data) >= 8 {
				p.Rx = binary.NativeEndian.Uint64(a.data)
			}
		case wgpeerATxBytes:
			if len(a.data) >= 8 {
				p.Tx = binary.NativeEndian.Uint64(a.data)
			}
		case wgpeerAAllowedIPs:
			for _, ip := range attrs(a.data) {
				if pf, ok := allowedIP(ip.data); ok {
					p.AllowedIPs = append(p.AllowedIPs, pf.String())
				}
			}
		}
	}
	return p
}

func allowedIP(b []byte) (netip.Prefix, bool) {
	var fam uint16
	var addr []byte
	bits := -1
	for _, a := range attrs(b) {
		switch a.typ {
		case wgallowedipAFamily:
			if len(a.data) >= 2 {
				fam = binary.NativeEndian.Uint16(a.data)
			}
		case wgallowedipAIPAddr:
			addr = a.data
		case wgallowedipACidrMask:
			if len(a.data) >= 1 {
				bits = int(a.data[0])
			}
		}
	}
	ip, ok := netip.AddrFromSlice(addr)
	if !ok || bits < 0 || (fam == afInet) != ip.Is4() {
		return netip.Prefix{}, false
	}
	pf, err := ip.Prefix(bits)
	return pf, err == nil
}

// sockaddr reads a struct sockaddr_in or sockaddr_in6 (port and address in
// network byte order).
func sockaddr(b []byte) (netip.AddrPort, bool) {
	if len(b) < 2 {
		return netip.AddrPort{}, false
	}
	switch binary.NativeEndian.Uint16(b[0:2]) {
	case afInet:
		if len(b) < 8 {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[2:4])), true
	case afInet6:
		if len(b) < 24 {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])).Unmap(), binary.BigEndian.Uint16(b[2:4])), true
	}
	return netip.AddrPort{}, false
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// mergeDevices joins the parts of a dump: one device can span messages.
func mergeDevices(parts []report.WGDevice) []report.WGDevice {
	var out []report.WGDevice
	idx := map[string]int{}
	for _, d := range parts {
		if i, ok := idx[d.Name]; ok {
			out[i].Peers = append(out[i].Peers, d.Peers...)
			if out[i].PublicKey == "" {
				out[i].PublicKey = d.PublicKey
			}
			continue
		}
		idx[d.Name] = len(out)
		if d.Peers == nil {
			d.Peers = []report.WGPeer{}
		}
		out = append(out, d)
	}
	return out
}

var errNoWireGuard = fmt.Errorf("wireguard: no such netlink family")
