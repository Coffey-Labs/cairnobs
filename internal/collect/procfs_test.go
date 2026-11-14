package collect

import (
	"math"
	"strings"
	"testing"
)

func TestParseStat(t *testing.T) {
	const stat = `cpu  4705 356 584 3699 23 23 0 0 10 2
cpu0 1393 280 260 1820 7 13 0 0 0 0
intr 1462898 0 0
`
	got, err := ParseStat(strings.NewReader(stat))
	if err != nil {
		t.Fatal(err)
	}
	// total = 4705+356+584+3699+23+23+0+0 (guest columns are already in user/nice)
	want := CPUTimes{Busy: 9390 - 3699 - 23, Total: 9390}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := ParseStat(strings.NewReader("intr 1\n")); err == nil {
		t.Fatal("want an error without a cpu line")
	}
}

func TestCPUPercent(t *testing.T) {
	prev := CPUTimes{Busy: 100, Total: 1000}
	if got := CPUPercent(prev, CPUTimes{Busy: 150, Total: 1100}); got != 50 {
		t.Fatalf("got %v, want 50", got)
	}
	// counters that go backwards (a reboot between samples) read as idle, not negative
	if got := CPUPercent(prev, CPUTimes{Busy: 10, Total: 50}); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	const mi = `MemTotal:       16318456 kB
MemFree:         1203424 kB
MemAvailable:    9876544 kB
SwapTotal:       8388604 kB
SwapFree:        8388000 kB
`
	m, err := ParseMeminfo(strings.NewReader(mi))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16318456*1024 || m.Available != 9876544*1024 || m.SwapTotal != 8388604*1024 || m.SwapFree != 8388000*1024 {
		t.Fatalf("got %+v", m)
	}
	if _, err := ParseMeminfo(strings.NewReader("MemFree: 1 kB\n")); err == nil {
		t.Fatal("want an error without MemTotal")
	}
}

func TestParseLoadavgUptime(t *testing.T) {
	l, err := ParseLoadavg(strings.NewReader("0.52 0.58 0.59 1/1234 5678\n"))
	if err != nil || l != [3]float64{0.52, 0.58, 0.59} {
		t.Fatalf("load %v %v", l, err)
	}
	u, err := ParseUptime(strings.NewReader("12345.67 54321.00\n"))
	if err != nil || math.Abs(u-12345.67) > 1e-9 {
		t.Fatalf("uptime %v %v", u, err)
	}
}

func TestParseNetDev(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999   100    0    0    0     0          0         0  9999999   100    0    0    0     0       0          0
  eth0: 1000      10     0    0    0     0          0         0  2000      20     0    0    0     0       0          0
wg0: 300     3      0    0    0     0          0         0  400       4      0    0    0     0       0          0
docker0: 5555     5      0    0    0     0          0         0  5555      5      0    0    0     0       0          0
vethab12: 777     7      0    0    0     0          0         0  777       7      0    0    0     0       0          0
br-1a2b: 888      8      0    0    0     0          0         0  888       8      0    0    0     0       0          0
`
	rx, tx, err := ParseNetDev(strings.NewReader(dev))
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1300 || tx != 2400 {
		t.Fatalf("got rx=%d tx=%d, want 1300/2400 (eth0 + wg0 only)", rx, tx)
	}
}

func TestParseMounts(t *testing.T) {
	const mounts = `proc /proc proc rw,nosuid 0 0
/dev/md2 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw 0 0
/dev/sda1 /srv ext4 rw,noatime 0 0
/dev/md2 /var/lib/docker ext4 rw,relatime 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
/dev/nvme0n1p2 /mnt/my\040disk btrfs rw 0 0
`
	got, err := ParseMounts(strings.NewReader(mounts))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Device: "/dev/md2", Path: "/", FS: "ext4"},
		{Device: "/dev/sda1", Path: "/srv", FS: "ext4"},
		{Device: "/dev/nvme0n1p2", Path: "/mnt/my disk", FS: "btrfs"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mount %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestOSRelease(t *testing.T) {
	if got := OSRelease(strings.NewReader("NAME=\"Debian\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n")); got != "Debian GNU/Linux 13 (trixie)" {
		t.Fatalf("got %q", got)
	}
}
