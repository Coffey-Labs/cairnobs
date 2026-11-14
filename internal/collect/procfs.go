// Package collect reads a Linux host's own numbers from /proc and statfs.
// The parsers take an io.Reader so they can be tested against fixtures.
package collect

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// CPUTimes is the aggregate "cpu" line of /proc/stat, in clock ticks.
type CPUTimes struct{ Busy, Total uint64 }

// ParseStat reads the aggregate CPU line of /proc/stat.
func ParseStat(r io.Reader) (CPUTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var total, idle uint64
		// user nice system idle iowait irq softirq steal [guest guest_nice];
		// guest time is already counted in user and nice.
		for i, s := range f[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return CPUTimes{}, fmt.Errorf("/proc/stat: %w", err)
			}
			total += v
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return CPUTimes{Busy: total - idle, Total: total}, nil
	}
	return CPUTimes{}, errors.New("/proc/stat: no cpu line")
}

// CPUPercent is the busy share between two samples.
func CPUPercent(prev, cur CPUTimes) float64 {
	if cur.Total <= prev.Total || cur.Busy < prev.Busy {
		return 0
	}
	return 100 * float64(cur.Busy-prev.Busy) / float64(cur.Total-prev.Total)
}

// ParseMeminfo reads /proc/meminfo into bytes.
func ParseMeminfo(r io.Reader) (report.Mem, error) {
	var m report.Mem
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		var dst *uint64
		switch f[0] {
		case "MemTotal:":
			dst = &m.Total
		case "MemAvailable:":
			dst = &m.Available
		case "SwapTotal:":
			dst = &m.SwapTotal
		case "SwapFree:":
			dst = &m.SwapFree
		default:
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return m, fmt.Errorf("/proc/meminfo %s: %w", f[0], err)
		}
		if len(f) > 2 && f[2] == "kB" {
			v *= 1024
		}
		*dst = v
	}
	if m.Total == 0 {
		return m, errors.New("/proc/meminfo: no MemTotal")
	}
	return m, nil
}

// ParseLoadavg reads the three load averages from /proc/loadavg.
func ParseLoadavg(r io.Reader) ([3]float64, error) {
	var l [3]float64
	b, err := io.ReadAll(r)
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return l, errors.New("/proc/loadavg: too short")
	}
	for i := range 3 {
		if l[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return l, fmt.Errorf("/proc/loadavg: %w", err)
		}
	}
	return l, nil
}

// ParseUptime reads seconds since boot from /proc/uptime.
func ParseUptime(r io.Reader) (float64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return 0, errors.New("/proc/uptime: empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

// virtualIface reports interfaces whose traffic is already counted on a
// real one (or never leaves the host): loopback, container plumbing.
func virtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "br-", "docker", "virbr", "cni", "flannel"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ParseNetDev sums received and sent bytes over the non-virtual interfaces
// of /proc/net/dev.
func ParseNetDev(r io.Reader) (rx, tx uint64, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if virtualIface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, fmt.Errorf("/proc/net/dev %s: bad counters", name)
		}
		rx += r
		tx += t
	}
	return rx, tx, sc.Err()
}

// Mount is one line of /proc/self/mounts.
type Mount struct{ Device, Path, FS string }

// realFS are filesystems that hold data we care about; everything else
// (proc, tmpfs, overlay, cgroup, ...) is skipped.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "f2fs": true, "bcachefs": true,
}

// ParseMounts returns real filesystems, one per device: the first mount
// listed wins, so bind mounts and btrfs subvolumes don't count twice.
func ParseMounts(r io.Reader) ([]Mount, error) {
	var out []Mount
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		dev := unescape(f[0])
		if seen[dev] {
			continue
		}
		seen[dev] = true
		out = append(out, Mount{Device: dev, Path: unescape(f[1]), FS: f[2]})
	}
	return out, sc.Err()
}

// unescape undoes the octal escapes (\040 for a space) in mount fields.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// OSRelease returns PRETTY_NAME from an os-release file.
func OSRelease(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
