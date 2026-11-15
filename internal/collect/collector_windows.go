package collect

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Windows has no /proc: the same numbers come from kernel32 and iphlpapi
// (docs/windows.md). A few calls x/sys/windows doesn't wrap are loaded
// here; none need privileges.
var (
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	iphlpapi                   = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetTickCount64         = kernel32.NewProc("GetTickCount64")
	procGetSystemTimes         = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemFirmwareTable = kernel32.NewProc("GetSystemFirmwareTable")
	procGetIfTable2            = iphlpapi.NewProc("GetIfTable2")
)

// Collector takes host samples. CPU and network are rates, so it keeps the
// previous sample; the first call reports them as zero.
type Collector struct {
	prevCPU  CPUTimes
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time

	machine      string // the SMBIOS UUID, read once
	machineKnown bool
}

// Collect fills the host fields of r. A field it can't read is left at its
// zero value and the error is returned alongside; the rest still counts.
// Windows has no load average: Load stays zero.
func (c *Collector) Collect(r *report.Report, now time.Time) []error {
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	r.CPUs = runtime.NumCPU()
	ms, _, _ := procGetTickCount64.Call()
	r.Uptime = float64(ms) / 1000
	var err error
	r.Mem, err = memory()
	keep(err)
	r.OS, r.Kernel = osVersion()

	cpu, cpuErr := cpuTimes()
	keep(cpuErr)
	rx, tx, netErr := netBytes()
	keep(netErr)
	if !c.prevTime.IsZero() {
		if cpuErr == nil {
			r.CPU = CPUPercent(c.prevCPU, cpu)
		}
		if secs := now.Sub(c.prevTime).Seconds(); netErr == nil && secs > 0 && rx >= c.prevRx && tx >= c.prevTx {
			r.Net = report.Net{RxBps: float64(rx-c.prevRx) / secs, TxBps: float64(tx-c.prevTx) / secs}
		}
	}
	c.prevCPU, c.prevRx, c.prevTx, c.prevTime = cpu, rx, tx, now

	var disks []report.Disk
	disks, err = volumes()
	keep(err)
	r.Disks = disks

	if !c.machineKnown {
		u, err := machineUUID()
		keep(err)
		c.machine, c.machineKnown = u, true // asked once: it doesn't change
	}
	r.Machine = c.machine
	return errs
}

func filetime(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// cpuTimes: kernel time includes idle time, so busy is kernel+user-idle.
func cpuTimes() (CPUTimes, error) {
	var idle, kern, user windows.Filetime
	if ok, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user))); ok == 0 {
		return CPUTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	total := filetime(kern) + filetime(user)
	return CPUTimes{Busy: total - filetime(idle), Total: total}, nil
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// memory: the commit limit beyond physical memory is the page file,
// reported as swap.
func memory() (report.Mem, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); ok == 0 {
		return report.Mem{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	out := report.Mem{Total: m.TotalPhys, Available: m.AvailPhys}
	if m.TotalPageFile > m.TotalPhys {
		// what's committed beyond what's in memory is roughly what's paged out
		out.SwapTotal = m.TotalPageFile - m.TotalPhys
		committed, inMemory := m.TotalPageFile-m.AvailPageFile, m.TotalPhys-m.AvailPhys
		paged := uint64(0)
		if committed > inMemory {
			paged = min(committed-inMemory, out.SwapTotal)
		}
		out.SwapFree = out.SwapTotal - paged
	}
	return out, nil
}

// osVersion is the product name with its release ("Windows Server 2025
// Datacenter Evaluation 24H2") and the kernel's version and build
// ("10.0.26100.1742").
func osVersion() (osName, kernel string) {
	v := windows.RtlGetVersion()
	kernel = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "Windows", kernel
	}
	defer k.Close()
	if ubr, _, err := k.GetIntegerValue("UBR"); err == nil {
		kernel += fmt.Sprintf(".%d", ubr)
	}
	osName, _, _ = k.GetStringValue("ProductName")
	// Windows 11 still says "Windows 10" in ProductName; the build tells.
	if strings.HasPrefix(osName, "Windows 10") && v.BuildNumber >= 22000 && v.ProductType == 1 {
		osName = "Windows 11" + strings.TrimPrefix(osName, "Windows 10")
	}
	if osName == "" {
		osName = "Windows"
	}
	if dv, _, err := k.GetStringValue("DisplayVersion"); err == nil && dv != "" {
		osName += " " + dv
	}
	return osName, kernel
}

// volumes are the fixed drives, by letter. Removable, network and optical
// drives are left out as tmpfs and NFS are on Linux.
func volumes() ([]report.Disk, error) {
	buf := make([]uint16, 256)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil {
		return nil, fmt.Errorf("GetLogicalDriveStrings: %w", err)
	}
	var disks []report.Disk
	var errs []error
	for _, root := range strings.Split(windows.UTF16ToString(buf[:n]), "\x00") {
		if root == "" {
			continue
		}
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		label := make([]uint16, windows.MAX_PATH+1)
		fsName := make([]uint16, windows.MAX_PATH+1)
		if err := windows.GetVolumeInformation(p, &label[0], uint32(len(label)), nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
			continue
		}
		var avail, total, free uint64
		if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
			continue
		}
		disks = append(disks, report.Disk{
			Mount: root, Device: windows.UTF16ToString(label), FS: windows.UTF16ToString(fsName),
			Size: total, Used: total - free, Avail: avail,
		})
	}
	return disks, errors.Join(errs...)
}

// netBytes adds up the hardware interfaces' byte counters: not loopback,
// not the filter drivers stacked on an adapter, not Hyper-V's virtual
// switches, which would count the same traffic again (as Linux leaves out
// bridges and veths).
func netBytes() (rx, tx uint64, err error) {
	var t *windows.MibIfTable2
	if rc, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&t))); rc != 0 {
		return 0, 0, fmt.Errorf("GetIfTable2: %w", windows.Errno(rc))
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	rows := unsafe.Slice(&t.Table[0], t.NumEntries)
	for i := range rows {
		row := &rows[i]
		const hardware, filter = 0x1, 0x2
		if row.InterfaceAndOperStatusFlags&hardware == 0 || row.InterfaceAndOperStatusFlags&filter != 0 {
			continue
		}
		if row.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		rx += row.InOctets
		tx += row.OutOctets
	}
	return rx, tx, nil
}

// machineUUID is the SMBIOS system UUID (type 1), as Linux shows it in
// /sys/class/dmi/id/product_uuid, so a Windows VM links to the hypervisor
// that runs it.
func machineUUID() (string, error) {
	const rsmb = 'R'<<24 | 'S'<<16 | 'M'<<8 | 'B'
	n, _, err := procGetSystemFirmwareTable.Call(rsmb, 0, 0, 0)
	if n == 0 {
		return "", fmt.Errorf("GetSystemFirmwareTable: %w", err)
	}
	buf := make([]byte, n)
	if got, _, err := procGetSystemFirmwareTable.Call(rsmb, 0, uintptr(unsafe.Pointer(&buf[0])), n); got == 0 || got > n {
		return "", fmt.Errorf("GetSystemFirmwareTable: %w", err)
	}
	u, ok := SMBIOSUUID(buf)
	if !ok {
		return "", errors.New("SMBIOS: no system UUID")
	}
	return report.MachineUUID(u), nil
}
