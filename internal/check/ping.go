package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/cairnobs/cairnobs/internal/report"
)

// pings sent per run, this far apart.
const (
	pings   = 3
	pingGap = 200 * time.Millisecond
)

// runPing sends three echo requests over an unprivileged ICMP socket
// (Linux allows them when net.ipv4.ping_group_range includes the agent's
// group; Debian's default does). One reply is enough to pass; loss and
// the average round trip are reported.
func runPing(ctx context.Context, s Spec, r *report.CheckResult) error {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, s.Ping)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%s has no address", s.Ping)
	}
	ip := addrs[0].IP
	network, proto, echo, reply := "udp4", 1, icmp.Type(ipv4.ICMPTypeEcho), icmp.Type(ipv4.ICMPTypeEchoReply)
	listen := "0.0.0.0"
	if ip.To4() == nil {
		network, proto, echo, reply, listen = "udp6", 58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, "::"
	}
	conn, err := icmp.ListenPacket(network, listen)
	if err != nil {
		return fmt.Errorf("can't open a ping socket: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	dst := &net.UDPAddr{IP: ip}
	id := os.Getpid() & 0xffff // the kernel replaces it on unprivileged sockets; Seq is what matches
	sent := map[int]time.Time{}
	var total time.Duration
	got := 0
	buf := make([]byte, 1500)
	for seq := 1; seq <= pings; seq++ {
		msg := icmp.Message{Type: echo, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("cairn-obs")}}
		b, _ := msg.Marshal(nil)
		sent[seq] = time.Now()
		if _, err := conn.WriteTo(b, dst); err != nil {
			return err
		}
		// Collect replies until the next ping is due (or, after the last, until the deadline).
		wait := time.Now().Add(pingGap)
		if seq == pings {
			wait = time.Now().Add(time.Second)
			if dl, ok := ctx.Deadline(); ok && dl.Before(wait) {
				wait = dl
			}
		}
		for got < pings {
			_ = conn.SetReadDeadline(wait)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return err
			}
			m, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil || m.Type != reply {
				continue
			}
			e, ok := m.Body.(*icmp.Echo)
			if !ok {
				continue
			}
			if at, ok := sent[e.Seq]; ok {
				total += time.Since(at)
				delete(sent, e.Seq)
				got++
			}
		}
	}
	r.Loss = 100 * (pings - got) / pings
	if got == 0 {
		return fmt.Errorf("no reply from %s", ip)
	}
	r.Ms = float64((total / time.Duration(got)).Microseconds()) / 1000
	return nil
}
