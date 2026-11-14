//go:build linux

package netmap

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A fake /proc: pids 10 (sshd, a unit) and 11 (a shell) share the host's
// network namespace; pid 20 is a container's process in its own.
func TestSamplerFixture(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(p, target string) {
		t.Helper()
		p = filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	const hdr = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	write("proc/10/ns/net", "")
	if err := os.MkdirAll(filepath.Join(root, "proc/11/ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "proc/10/ns/net"), filepath.Join(root, "proc/11/ns/net")); err != nil {
		t.Fatal(err) // same namespace: same inode
	}
	write("proc/10/net/tcp", hdr+
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 500 1 0 100 0 0 10 0\n"+
		"   1: 0200000A:0016 0300000A:D431 01 00000000:00000000 00:00000000 00000000     0        0 501 1 0 100 0 0 10 0\n"+
		"   2: 0200000A:9C40 0900000A:1538 01 00000000:00000000 00:00000000 00000000     0        0 502 1 0 100 0 0 10 0\n")
	write("proc/10/cgroup", "0::/system.slice/ssh.service\n")
	write("proc/10/comm", "sshd\n")
	link("proc/10/fd/3", "socket:[500]")
	link("proc/10/fd/4", "socket:[501]")
	link("proc/10/fd/5", "/dev/null")
	write("proc/11/cgroup", "0::/user.slice/user-1000.slice/session-2.scope\n")
	write("proc/11/comm", "psql\n")
	link("proc/11/fd/7", "socket:[502]")
	write("proc/20/ns/net", "")
	write("proc/20/net/tcp6", hdr+
		"   0: 00000000000000000000000000000000:1538 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 600 1 0 100 0 0 10 0\n")
	write("proc/20/cgroup", "0::/system.slice/docker-"+string(slices.Repeat([]byte("c"), 64))+".scope\n")
	link("proc/20/fd/3", "socket:[600]")
	write("proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\neth0\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n")

	s := &Sampler{Root: root, Interfaces: func() ([]net.Interface, error) { return nil, nil }}
	m := s.Sample()
	if m.Error != "" || m.Gateway != "10.0.0.1" {
		t.Errorf("error %q, gateway %q", m.Error, m.Gateway)
	}
	wantL := []report.Listener{
		{Owner: "unit:ssh.service", Proto: "tcp", Addr: "*", Port: 22},
		{Owner: "container:cccccccccccc", Proto: "tcp", Addr: "*", Port: 5432},
	}
	if !slices.Equal(m.Listen, wantL) {
		t.Errorf("listen = %v", m.Listen)
	}
	wantC := []report.Conn{
		{Owner: "unit:ssh.service", Dir: "in", Remote: "10.0.0.3", Port: 22, Count: 1},
		{Owner: "process:psql", Dir: "out", Remote: "10.0.0.9", Port: 5432, Count: 1},
	}
	if !slices.Equal(m.Conns, wantC) {
		t.Errorf("conns = %v", m.Conns)
	}
}
