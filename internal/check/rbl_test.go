package check

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS answers A queries: listed.test lists 192.0.2.66, refuse.test
// refuses everyone (Spamhaus-style), everything else is NXDOMAIN.
func fakeDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			name := q.Name.String()
			rh := dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true}
			var ip [4]byte
			switch {
			case q.Type != dnsmessage.TypeA:
				rh.RCode = dnsmessage.RCodeSuccess
			case name == "66.2.0.192.listed.test.":
				ip = [4]byte{127, 0, 0, 2}
			case strings.HasSuffix(name, ".refuse.test."):
				ip = [4]byte{127, 255, 255, 254}
			default:
				rh.RCode = dnsmessage.RCodeNameError
			}
			b := dnsmessage.NewBuilder(nil, rh)
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if ip != [4]byte{} {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: ip})
			}
			out, _ := b.Finish()
			pc.WriteTo(out, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestRBL(t *testing.T) {
	srv := fakeDNS(t)
	ctx := context.Background()
	lists := []string{"listed.test", "clean.test", "refuse.test"}
	r := Run(ctx, spec(t, Spec{ID: "mx", RBL: "192.0.2.66", Lists: lists, Server: srv}))
	if r.OK || r.Error != "listed on listed.test (127.0.0.2)" {
		t.Fatalf("listed: %+v", r)
	}
	r = Run(ctx, spec(t, Spec{ID: "mx", RBL: "192.0.2.10", Lists: lists, Server: srv}))
	if !r.OK || r.Answer != "clean on 2 of 3; won't answer us: refuse.test" {
		t.Fatalf("clean: %+v", r)
	}
	r = Run(ctx, spec(t, Spec{ID: "mx", RBL: "192.0.2.10", Lists: []string{"refuse.test"}, Server: srv}))
	if r.OK || !strings.Contains(r.Error, "no block list could be asked") {
		t.Fatalf("all refused: %+v", r)
	}
	if s := spec(t, Spec{ID: "d", RBL: "192.0.2.1"}); len(s.Lists) != len(DefaultLists) || s.Every.Minutes() != 30 {
		t.Fatalf("defaults: %+v", s)
	}
	if err := (&Spec{ID: "v6", RBL: "2001:db8::25"}).Check(); err == nil {
		t.Fatal("accepted IPv6")
	}
}
