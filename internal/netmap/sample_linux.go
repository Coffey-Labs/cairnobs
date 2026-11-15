//go:build linux

package netmap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Sampler reads the map sample. Root is prefixed to /proc and /sys paths
// (fixtures in tests); empty on a real host.
type Sampler struct {
	Root string
	// Interfaces lists the host's interfaces; nil means net.Interfaces.
	// Tests give their own.
	Interfaces func() ([]net.Interface, error)
	// WireGuard dumps the WireGuard devices; nil means the kernel's.
	WireGuard func() ([]report.WGDevice, error)
}

func (s *Sampler) path(p string) string { return filepath.Join(s.Root, p) }

// Sample takes one map sample. What can't be read is named in Error and
// the rest still counts.
func (s *Sampler) Sample() *report.Map {
	m := &report.Map{}
	var errs []string
	keep := func(what string, err error) {
		if err != nil {
			errs = append(errs, what+": "+err.Error())
		}
	}
	var wgNames []string
	m.Interfaces, wgNames, _ = s.interfaces(keep)
	if f, err := os.Open(s.path("/proc/net/route")); err == nil {
		m.Gateway = ParseRoute(f)
		f.Close()
	}
	if len(wgNames) > 0 {
		dump := s.WireGuard
		if dump == nil {
			dump = wireGuardDump
		}
		devs, err := dump()
		keep("wireguard", err)
		m.WireGuard = devs
	}
	socks, owner, err := s.sockets()
	keep("sockets", err)
	// The host's own network namespace is init's.
	var hostNS uint64
	if st, err := os.Stat(s.path("/proc/1/ns/net")); err == nil {
		hostNS = st.Sys().(*syscall.Stat_t).Ino
	}
	m.Listen, m.Conns, m.Cut = Shrink(socks, hostNS, owner)
	m.Error = strings.Join(errs, "; ")
	return m
}

func (s *Sampler) interfaces(keep func(string, error)) (out []report.Interface, wg []string, err error) {
	list := s.Interfaces
	if list == nil {
		list = net.Interfaces
	}
	ifs, err := list()
	if err != nil {
		keep("interfaces", err)
		return nil, nil, err
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		var addrs []string
		as, _ := ifc.Addrs()
		for _, a := range as {
			pf, err := netip.ParsePrefix(a.String())
			if err != nil || pf.Addr().IsLinkLocalUnicast() {
				continue
			}
			addrs = append(addrs, pf.String())
		}
		devtype := ""
		if b, err := os.ReadFile(s.path("/sys/class/net/" + ifc.Name + "/uevent")); err == nil {
			for line := range strings.Lines(string(b)) {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DEVTYPE="); ok {
					devtype = v
				}
			}
		}
		_, devErr := os.Stat(s.path("/sys/class/net/" + ifc.Name + "/device"))
		kind := InterfaceKind(ifc.Name, devtype, devErr == nil)
		if kind == "wireguard" {
			wg = append(wg, ifc.Name)
		}
		if len(addrs) == 0 {
			continue
		}
		out = append(out, report.Interface{Name: ifc.Name, Kind: kind, Addrs: addrs, Up: ifc.Flags&net.FlagUp != 0})
	}
	slices.SortFunc(out, func(a, b report.Interface) int { return strings.Compare(a.Name, b.Name) })
	if len(out) > MaxInterfaces {
		out = out[:MaxInterfaces]
	}
	return out, wg, nil
}

// sockets reads the socket tables of every network namespace (once each,
// through its first process) and finds each socket's holder from the
// processes' file descriptors.
func (s *Sampler) sockets() ([]NSSocket, func(uint64) string, error) {
	ents, err := os.ReadDir(s.path("/proc"))
	if err != nil {
		return nil, func(uint64) string { return "" }, err
	}
	var socks []NSSocket
	seenNS := map[uint64]bool{}
	holder := map[uint64]int{} // socket inode → pid
	var firstErr error
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := s.path("/proc/" + e.Name())
		if st, err := os.Stat(dir + "/ns/net"); err == nil {
			ns := st.Sys().(*syscall.Stat_t).Ino
			if !seenNS[ns] {
				seenNS[ns] = true
				for _, t := range [...]struct{ file, proto string }{{"tcp", "tcp"}, {"tcp6", "tcp"}, {"udp", "udp"}, {"udp6", "udp"}} {
					f, err := os.Open(dir + "/net/" + t.file)
					if err != nil {
						continue // no IPv6, or the process is gone
					}
					list, err := ParseNetTable(f, t.proto)
					f.Close()
					if err != nil && firstErr == nil {
						firstErr = err
					}
					for _, sk := range list {
						socks = append(socks, NSSocket{Socket: sk, NS: ns})
					}
				}
			}
		}
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue // gone, or not ours to read (the helper runs as root)
		}
		for _, fd := range fds {
			l, err := os.Readlink(dir + "/fd/" + fd.Name())
			if err != nil {
				continue
			}
			if v, ok := strings.CutPrefix(l, "socket:["); ok {
				if ino, err := strconv.ParseUint(strings.TrimSuffix(v, "]"), 10, 64); err == nil {
					if _, ok := holder[ino]; !ok {
						holder[ino] = pid
					}
				}
			}
		}
	}
	owners := map[int]string{}
	owner := func(ino uint64) string {
		pid, ok := holder[ino]
		if !ok {
			return ""
		}
		if o, ok := owners[pid]; ok {
			return o
		}
		dir := s.path("/proc/" + strconv.Itoa(pid))
		o := ""
		if b, err := os.ReadFile(dir + "/cgroup"); err == nil {
			o = OwnerFromCgroup(string(b))
		}
		if o == "" {
			if b, err := os.ReadFile(dir + "/comm"); err == nil {
				if c := strings.TrimSpace(string(b)); c != "" {
					o = "process:" + c
				}
			}
		}
		owners[pid] = o
		return o
	}
	if len(seenNS) == 0 && firstErr == nil {
		firstErr = errors.New("no network namespaces readable")
	}
	return socks, owner, firstErr
}

// wireGuardDump asks the kernel for every WireGuard device over generic
// netlink. It needs CAP_NET_ADMIN, which the root helper has.
func wireGuardDump() ([]report.WGDevice, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_GENERIC)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}
	tv := syscall.Timeval{Sec: 2}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	// The family's id first.
	const genlIDCtrl, ctrlCmdGetFamily, ctrlAttrFamilyID, ctrlAttrFamilyName = 0x10, 3, 1, 2
	name := append([]byte("wireguard"), 0)
	if err := nlSend(fd, genlIDCtrl, syscall.NLM_F_REQUEST, ctrlCmdGetFamily, 1, nlAttr(ctrlAttrFamilyName, name)); err != nil {
		return nil, err
	}
	var family uint16
	err = nlRecv(fd, func(b []byte) {
		for _, a := range attrs(b) {
			if a.typ == ctrlAttrFamilyID && len(a.data) >= 2 {
				family = binary.NativeEndian.Uint16(a.data)
			}
		}
	})
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, errNoWireGuard
		}
		return nil, err
	}
	if family == 0 {
		return nil, errNoWireGuard
	}

	// One dump per device: the kernel wants a name or an index each time.
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var parts []report.WGDevice
	for _, ifc := range ifs {
		b, err := os.ReadFile("/sys/class/net/" + ifc.Name + "/uevent")
		if err != nil || !strings.Contains(string(b), "DEVTYPE=wireguard") {
			continue
		}
		if err := nlSend(fd, family, syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, wgCmdGetDevice, wgGenlVersion,
			nlAttr(wgdeviceAIfname, append([]byte(ifc.Name), 0))); err != nil {
			return nil, err
		}
		if err := nlRecv(fd, func(b []byte) { parts = append(parts, parseWGDevice(b)) }); err != nil {
			return nil, fmt.Errorf("%s: %w", ifc.Name, err)
		}
	}
	return mergeDevices(parts), nil
}

func nlAttr(typ uint16, data []byte) []byte {
	l := 4 + len(data)
	b := make([]byte, (l+3)&^3)
	binary.NativeEndian.PutUint16(b[0:2], uint16(l))
	binary.NativeEndian.PutUint16(b[2:4], typ)
	copy(b[4:], data)
	return b
}

func nlSend(fd int, family uint16, flags uint16, cmd, version byte, payload []byte) error {
	b := make([]byte, 16+4+len(payload))
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:6], family)
	if flags&syscall.NLM_F_DUMP == 0 {
		flags |= syscall.NLM_F_ACK // a dump ends in NLMSG_DONE instead
	}
	binary.NativeEndian.PutUint16(b[6:8], flags)
	binary.NativeEndian.PutUint32(b[8:12], 1) // sequence
	b[16], b[17] = cmd, version
	copy(b[20:], payload)
	return syscall.Sendto(fd, b, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK})
}

// nlRecv reads answers until the dump or the ack ends, handing each
// message's attributes (after the genetlink header) to f.
func nlRecv(fd int, f func([]byte)) error {
	buf := make([]byte, 1<<16)
	defer wipe(buf)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return nil
			case syscall.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					if e := int32(binary.NativeEndian.Uint32(m.Data[0:4])); e != 0 {
						return syscall.Errno(-e)
					}
				}
				if m.Header.Flags&syscall.NLM_F_MULTI == 0 {
					return nil // the ack after a single answer
				}
			default:
				if len(m.Data) >= 4 {
					f(m.Data[4:])
				}
			}
		}
	}
}
