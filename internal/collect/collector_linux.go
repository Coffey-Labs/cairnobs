//go:build linux

package collect

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Collector takes host samples. CPU and network are rates, so it keeps the
// previous sample; the first call reports them as zero.
type Collector struct {
	// Root is prefixed to /proc and /etc paths; empty on a real host.
	Root string

	prevCPU  CPUTimes
	prevRx   uint64
	prevTx   uint64
	prevTime time.Time
}

func (c *Collector) open(p string) (*os.File, error) { return os.Open(filepath.Join(c.Root, p)) }

func (c *Collector) read(p string, parse func(io.Reader) error) error {
	f, err := c.open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return parse(f)
}

// Collect fills the host fields of r. A field it can't read is left at its
// zero value and the error is returned alongside; the rest still counts.
func (c *Collector) Collect(r *report.Report, now time.Time) []error {
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	r.CPUs = runtime.NumCPU()

	keep(c.read("/proc/uptime", func(f io.Reader) (err error) { r.Uptime, err = ParseUptime(f); return }))
	keep(c.read("/proc/loadavg", func(f io.Reader) (err error) { r.Load, err = ParseLoadavg(f); return }))
	keep(c.read("/proc/meminfo", func(f io.Reader) (err error) { r.Mem, err = ParseMeminfo(f); return }))
	keep(c.read("/proc/sys/kernel/osrelease", func(f io.Reader) error {
		b, err := io.ReadAll(f)
		r.Kernel = strings.TrimSpace(string(b))
		return err
	}))
	if c.read("/etc/os-release", func(f io.Reader) error { r.OS = OSRelease(f); return nil }) != nil {
		_ = c.read("/usr/lib/os-release", func(f io.Reader) error { r.OS = OSRelease(f); return nil })
	}

	var cpu CPUTimes
	cpuErr := c.read("/proc/stat", func(f io.Reader) (err error) { cpu, err = ParseStat(f); return })
	keep(cpuErr)
	var rx, tx uint64
	netErr := c.read("/proc/net/dev", func(f io.Reader) (err error) { rx, tx, err = ParseNetDev(f); return })
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

	var mounts []Mount
	keep(c.read("/proc/self/mounts", func(f io.Reader) (err error) { mounts, err = ParseMounts(f); return }))
	r.Disks = r.Disks[:0]
	for _, m := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(filepath.Join(c.Root, m.Path), &st); err != nil {
			keep(err)
			continue
		}
		bs := uint64(st.Bsize)
		size, free, avail := st.Blocks*bs, st.Bfree*bs, st.Bavail*bs
		r.Disks = append(r.Disks, report.Disk{
			Mount: m.Path, Device: m.Device, FS: m.FS,
			Size: size, Used: size - free, Avail: avail,
		})
	}
	return errs
}
