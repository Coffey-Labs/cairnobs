package netmap

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/units"
)

// On Windows the agent takes the map sample itself, as its own service
// account: the socket tables with their processes, the adapters and the
// default gateway need no administrator (docs/windows.md). WireGuard's
// peers do, so they aren't read; its interfaces still show.

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	tcpTableOwnerPIDAll = 5 // TCP_TABLE_OWNER_PID_ALL
	udpTableOwnerPID    = 1 // UDP_TABLE_OWNER_PID
)

// Local takes one map sample of this host. What can't be read is named in
// Error and the rest still counts.
func Local() *report.Map {
	m := &report.Map{}
	var errs []string
	keep := func(what string, err error) {
		if err != nil {
			errs = append(errs, what+": "+err.Error())
		}
	}
	var err error
	m.Interfaces, m.Gateway, err = adapters()
	keep("interfaces", err)

	var socks []NSSocket
	for _, t := range [...]struct {
		proto string
		v6    bool
	}{{"tcp", false}, {"tcp", true}, {"udp", false}, {"udp", true}} {
		b, err := winTable(t.proto, t.v6)
		if err != nil {
			keep(t.proto, err)
			continue
		}
		list, err := ParseWinTable(b, t.proto, t.v6)
		keep(t.proto, err)
		for _, s := range list {
			socks = append(socks, NSSocket{Socket: s})
		}
	}
	services, err := units.ByProcess()
	keep("services", err)
	exe, err := processNames()
	keep("processes", err)
	owner := func(pid uint64) string { return WinOwner(uint32(pid), services, exe) }
	m.Listen, m.Conns, m.Cut = Shrink(socks, 0, owner)
	m.Error = strings.Join(errs, "; ")
	return m
}

func winTable(proto string, v6 bool) ([]byte, error) {
	proc, class := procGetExtendedTcpTable, uintptr(tcpTableOwnerPIDAll)
	if proto == "udp" {
		proc, class = procGetExtendedUdpTable, udpTableOwnerPID
	}
	family := uintptr(windows.AF_INET)
	if v6 {
		family = windows.AF_INET6
	}
	size := uint32(16 << 10)
	for range 5 { // the table grows between the calls on a busy host
		b := make([]byte, size)
		rc, _, _ := proc.Call(uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&size)), 0, family, class, 0)
		switch windows.Errno(rc) {
		case 0:
			return b[:size], nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10
			continue
		default:
			return nil, windows.Errno(rc)
		}
	}
	return nil, errors.New("the table kept growing")
}

// processNames is every process's program name by ID, from a snapshot:
// no process needs opening, so none is out of reach.
func processNames() (map[uint32]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	out := map[uint32]string{}
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out[e.ProcessID] = windows.UTF16ToString(e.ExeFile[:])
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		err = nil
	}
	return out, err
}

// adapters are the interfaces with their addresses (link-local left out,
// as on Linux), and the IPv4 default gateway of the first that has one.
func adapters() ([]report.Interface, string, error) {
	size := uint32(32 << 10)
	var b []byte
	for range 5 {
		b = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_GATEWAYS|windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER,
			0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&b[0])), &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, "", fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		b = nil
	}
	if b == nil {
		return nil, "", errors.New("GetAdaptersAddresses: the list kept growing")
	}
	var out []report.Interface
	gateway := ""
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&b[0])); a != nil; a = a.Next {
		if a.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		i := report.Interface{
			Name: windows.UTF16PtrToString(a.FriendlyName),
			Kind: adapterKind(a.IfType, windows.UTF16PtrToString(a.Description)),
			Up:   a.OperStatus == windows.IfOperStatusUp,
		}
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			ip, ok := netip.AddrFromSlice(u.Address.IP())
			if !ok || ip.Unmap().IsLinkLocalUnicast() {
				continue
			}
			i.Addrs = append(i.Addrs, netip.PrefixFrom(ip.Unmap(), int(u.OnLinkPrefixLength)).String())
		}
		if len(i.Addrs) == 0 {
			continue // an interface with an address, as on Linux
		}
		if gateway == "" && i.Up {
			for g := a.FirstGatewayAddress; g != nil; g = g.Next {
				if ip, ok := netip.AddrFromSlice(g.Address.IP()); ok && ip.Unmap().Is4() && !ip.Unmap().IsUnspecified() {
					gateway = ip.Unmap().String()
					break
				}
			}
		}
		if len(out) < MaxInterfaces {
			out = append(out, i)
		}
	}
	return out, gateway, nil
}

// adapterKind sorts an adapter as InterfaceKind does Linux's.
func adapterKind(ifType uint32, description string) string {
	d := strings.ToLower(description)
	switch {
	case strings.Contains(d, "wireguard"):
		return "wireguard"
	case strings.Contains(d, "hyper-v virtual"), strings.Contains(d, "virtual ethernet"), strings.Contains(d, "vmware virtual"):
		return "virtual"
	case ifType == 131: // IF_TYPE_TUNNEL
		return "tun"
	case ifType == 6, ifType == 71: // Ethernet, Wi-Fi
		return "physical"
	}
	return "virtual"
}
