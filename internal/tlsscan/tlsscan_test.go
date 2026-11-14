package tlsscan

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func testTLS(t *testing.T, cn string) *tls.Config {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// serve answers each connection with greet (the plain-text part), then
// TLS when greet says yes.
func serve(t *testing.T, cfg *tls.Config, greet func(c net.Conn, r *bufio.Reader) bool) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if greet != nil && !greet(c, bufio.NewReader(c)) {
					return
				}
				_ = tls.Server(c, cfg).Handshake()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func lineIs(r *bufio.Reader, want string) bool {
	l, err := r.ReadString('\n')
	return err == nil && strings.HasPrefix(l, want)
}

func TestScan(t *testing.T) {
	ports := map[string]int{
		"tls": serve(t, testTLS(t, "web.example.org"), nil),
		"smtp": serve(t, testTLS(t, "mx.example.org"), func(c net.Conn, r *bufio.Reader) bool {
			io.WriteString(c, "220 mx ESMTP\r\n")
			if !lineIs(r, "EHLO") {
				return false
			}
			io.WriteString(c, "250-mx\r\n250-PIPELINING\r\n250 STARTTLS\r\n")
			if !lineIs(r, "STARTTLS") {
				return false
			}
			io.WriteString(c, "220 2.0.0 Ready\r\n")
			return true
		}),
		"imap": serve(t, testTLS(t, "imap.example.org"), func(c net.Conn, r *bufio.Reader) bool {
			io.WriteString(c, "* OK IMAP ready\r\n")
			if !lineIs(r, "a1 STARTTLS") {
				return false
			}
			io.WriteString(c, "a1 OK Begin TLS\r\n")
			return true
		}),
		"pop3": serve(t, testTLS(t, "pop.example.org"), func(c net.Conn, r *bufio.Reader) bool {
			io.WriteString(c, "+OK POP3\r\n")
			if !lineIs(r, "STLS") {
				return false
			}
			io.WriteString(c, "+OK\r\n")
			return true
		}),
		"postgres": serve(t, testTLS(t, "db.example.org"), func(c net.Conn, r *bufio.Reader) bool {
			b := make([]byte, 8)
			if _, err := io.ReadFull(r, b); err != nil || b[7] != 0x2f {
				return false
			}
			c.Write([]byte{'S'})
			return true
		}),
		"ldap": serve(t, testTLS(t, "ldap.example.org"), func(c net.Conn, r *bufio.Reader) bool {
			b := make([]byte, 31)
			if _, err := io.ReadFull(r, b); err != nil || b[5] != 0x77 {
				return false
			}
			// ExtendedResponse, message 1, success.
			c.Write([]byte{0x30, 0x0c, 0x02, 0x01, 0x01, 0x78, 0x07, 0x0a, 0x01, 0x00, 0x04, 0x00, 0x04, 0x00})
			return true
		}),
		"plain": serve(t, nil, func(c net.Conn, r *bufio.Reader) bool { io.WriteString(c, "hello\r\n"); return false }),
	}
	s := New("web1")
	s.ask = map[int]string{}
	var listen []report.Listener
	for p, port := range ports {
		if p != "tls" && p != "plain" {
			s.ask[port] = p
		}
		listen = append(listen, report.Listener{Proto: "tcp", Addr: "*", Port: port, Owner: "svc-" + p})
	}
	listen = append(listen, report.Listener{Proto: "udp", Addr: "*", Port: 9999}, report.Listener{Proto: "tcp", Addr: "*", Port: 22})
	now := time.Now()
	s.Update(listen, now)
	s.Round(context.Background(), now)
	s.Round(context.Background(), now) // seven ports: two rounds
	r := s.Report(now)
	if r == nil || len(r.List) != 6 {
		t.Fatalf("%+v", r)
	}
	got := map[string]report.CertSeen{}
	for _, c := range r.List {
		got[c.How] = c
	}
	for how, cn := range map[string]string{"tls": "web.example.org", "smtp": "mx.example.org", "imap": "imap.example.org", "pop3": "pop.example.org",
		"postgres": "db.example.org", "ldap": "ldap.example.org"} {
		c := got[how]
		if c.Cert.Subject != cn || c.Owner != "svc-"+how || c.Port != ports[how] || c.Cert.Key != "ECDSA P-256" || len(c.Cert.Fingerprint) != 64 {
			t.Errorf("%s: %+v", how, c)
		}
	}
	// Unchanged: not sent again until resend; a port gone is.
	if s.Report(now.Add(time.Minute)) != nil {
		t.Error("sent again unchanged")
	}
	var fewer []report.Listener
	for _, l := range listen {
		if l.Owner != "svc-tls" {
			fewer = append(fewer, l)
		}
	}
	s.Update(fewer, now.Add(2*time.Minute))
	if r := s.Report(now.Add(2 * time.Minute)); r == nil || len(r.List) != 5 {
		t.Errorf("after a port went: %+v", r)
	}
	if r := s.Report(now.Add(40 * time.Minute)); r == nil {
		t.Error("not resent after half an hour")
	}
	// Tried again a day later, not before.
	s.mu.Lock()
	for _, e := range s.tgts {
		if e.next.Before(now.Add(Every)) || e.next.After(now.Add(Every+time.Hour)) {
			t.Errorf("next try %v", e.next.Sub(now))
		}
	}
	s.mu.Unlock()
}

// A cluster agent's names: each tried on 443 with itself as SNI, and a
// wildcard skipped.
func TestNamed(t *testing.T) {
	shop, api := testTLS(t, "shop.example.org"), testTLS(t, "api.example.org")
	port := serve(t, &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if h.ServerName == "api.example.org" {
			return &api.Certificates[0], nil
		}
		return &shop.Certificates[0], nil
	}}, nil)
	s := New("cluster-a")
	var dialed []string
	s.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	now := time.Now()
	s.UpdateNames([]string{"shop.example.org", "api.example.org", "*.apps.example.org"}, now)
	s.Round(context.Background(), now)
	got := s.Named()
	if len(got) != 2 || got["api.example.org"].Cert.Subject != "api.example.org" || got["shop.example.org"].Cert.Subject != "shop.example.org" {
		t.Errorf("%+v", got)
	}
	if len(dialed) != 2 || dialed[0] != "api.example.org:443" {
		t.Errorf("dialed %q", dialed)
	}
	s.UpdateNames([]string{"api.example.org"}, now)
	if len(s.Named()) != 1 {
		t.Error("a name no longer listed is kept")
	}
}

// issue makes a certificate signed by parent's key (itself when parent is
// nil).
func issue(t *testing.T, cn string, ca bool, aia bool, parent *x509.Certificate, pkey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), IsCA: ca, BasicConstraintsValid: true}
	if !ca {
		tpl.DNSNames = []string{cn}
	}
	if aia {
		tpl.IssuingCertificateURL = []string{"http://ca.example.net/int.crt"}
	}
	if parent == nil {
		parent, pkey = tpl, k
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &k.PublicKey, pkey)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(der)
	return x, k
}

// Step 5: what's wrong with how a chain is served, from the chain alone.
func TestWrong(t *testing.T) {
	root, rk := issue(t, "Root", true, false, nil, nil)
	inter, ik := issue(t, "Intermediate", true, false, root, rk)
	other, _ := issue(t, "Other intermediate", true, false, root, rk)
	leaf, _ := issue(t, "shop.example.org", false, true, inter, ik)
	private, _ := issue(t, "admin.internal", false, false, root, rk) // a private CA's: no AIA
	self, _ := issue(t, "snakeoil", true, false, nil, nil)
	for _, c := range []struct {
		chain []*x509.Certificate
		name  string
		want  string
	}{
		{[]*x509.Certificate{leaf, inter}, "shop.example.org", ""},
		{[]*x509.Certificate{leaf}, "shop.example.org", "sent without its intermediate certificate"},
		{[]*x509.Certificate{leaf, other}, "", "sent with an intermediate that didn't sign it"},
		{[]*x509.Certificate{leaf, inter}, "api.example.org", "doesn't cover api.example.org"},
		{[]*x509.Certificate{private}, "", ""},
		{[]*x509.Certificate{self}, "", ""},
	} {
		if got := strings.Join(Wrong(c.chain, c.name, selfSigned(c.chain[0])), "; "); got != c.want {
			t.Errorf("%s+%d for %q: %q, want %q", c.chain[0].Subject.CommonName, len(c.chain)-1, c.name, got, c.want)
		}
	}

	// A port that served a CA-signed certificate and now serves a
	// self-signed one.
	snake := testTLS(t, "snakeoil")
	signed := issueTLS(t, root, rk, "admin.internal")
	var now = time.Now()
	which := signed
	port := serve(t, &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return which, nil }}, nil)
	s := New("web1")
	s.Update([]report.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: port}}, now)
	s.Round(context.Background(), now)
	which = &snake.Certificates[0]
	later := now.Add(Every + time.Hour)
	s.Round(context.Background(), later)
	r := s.Report(later)
	if r == nil || len(r.List) != 1 || strings.Join(r.List[0].Wrong, "; ") != "self-signed where a CA-signed certificate was" {
		t.Errorf("%+v", r)
	}
}

// issueTLS is a served certificate for cn signed by ca.
func issueTLS(t *testing.T, ca *x509.Certificate, cak *ecdsa.PrivateKey, cn string) *tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, cak)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: k}
}
