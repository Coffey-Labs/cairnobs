package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert is a certificate for 127.0.0.1 and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// fakeSMTP speaks just enough SMTP. It records the login and the message.
type fakeSMTP struct {
	addr      string
	starttls  bool
	got       chan string
	plainAuth chan string
}

func startSMTP(t *testing.T, cert tls.Certificate, implicit, starttls bool) *fakeSMTP {
	t.Helper()
	tc := &tls.Config{Certificates: []tls.Certificate{cert}}
	var ln net.Listener
	var err error
	if implicit {
		ln, err = tls.Listen("tcp", "127.0.0.1:0", tc)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String(), starttls: starttls, got: make(chan string, 1), plainAuth: make(chan string, 1)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r, w := bufio.NewReader(conn), conn
		say := func(s string) { w.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					f.got <- data.String()
					say("250 queued")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
			switch cmd {
			case "EHLO":
				w.Write([]byte("250-fake\r\n"))
				if f.starttls && !implicit {
					w.Write([]byte("250-STARTTLS\r\n"))
				}
				say("250 AUTH PLAIN")
			case "STARTTLS":
				say("220 go ahead")
				tconn := tls.Server(conn, tc)
				if tconn.Handshake() != nil {
					return
				}
				conn, r, w = tconn, bufio.NewReader(tconn), tconn
			case "AUTH":
				parts := strings.Fields(line)
				b, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
				f.plainAuth <- string(b)
				say("235 ok")
			case "MAIL", "RCPT":
				say("250 ok")
			case "DATA":
				inData = true
				say("354 go")
			case "QUIT":
				say("221 bye")
				return
			default:
				say("502 no")
			}
		}
	}()
	return f
}

func emailChannel(t *testing.T, addr string, pool *x509.CertPool) *email {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pw, []byte("s3cret-pass\n"), 0o600)
	ch, err := New(Config{ID: "mail", Type: "email", Min: "serious", SMTP: addr, PasswordFile: pw,
		From: "Cairn OBS <obs@example.test>", To: []string{"ops@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	e := ch.(*email)
	e.roots = pool
	return e
}

var sample = Notice{Kind: "firing", ID: "B-0007", Severity: "critical", Title: "mail.example.org SMTP (25) is down",
	Detail: "Failing from host2, host3: connection refused.", URL: "https://obs.example.test/checks", Since: time.Now()}

func TestEmailStartTLS(t *testing.T) {
	cert, pool := testCert(t)
	f := startSMTP(t, cert, false, true)
	e := emailChannel(t, f.addr, pool)
	if err := e.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if auth := <-f.plainAuth; auth != "\x00obs@example.test\x00s3cret-pass" {
		t.Fatalf("login %q", auth)
	}
	msg := <-f.got
	for _, want := range []string{"Subject: [Cairn OBS] Critical: mail.example.org SMTP (25) is down", "Auto-Submitted: auto-generated",
		"From: \"Cairn OBS\" <obs@example.test>", "Failing from host2, host3", "https://obs.example.test/checks", "Incident B-0007"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestEmailImplicitTLS(t *testing.T) {
	cert, pool := testCert(t)
	f := startSMTP(t, cert, true, false)
	// Port 465 means TLS from the first byte; the fake listens elsewhere, so set it directly.
	e := emailChannel(t, f.addr, pool)
	e.implicit = true
	if err := e.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if msg := <-f.got; !strings.Contains(msg, "mail.example.org SMTP (25) is down") {
		t.Fatalf("message: %s", msg)
	}
}

func TestEmailRefusesCleartext(t *testing.T) {
	cert, pool := testCert(t)
	f := startSMTP(t, cert, false, false) // no STARTTLS on offer
	e := emailChannel(t, f.addr, pool)
	err := e.Send(context.Background(), sample)
	if err == nil || !strings.Contains(err.Error(), "refusing to send in the clear") {
		t.Fatalf("sent without TLS: %v", err)
	}
	select {
	case a := <-f.plainAuth:
		t.Fatalf("password went over plain text: %q", a)
	default:
	}
}

func TestEmailConfig(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pw, []byte("x"), 0o600)
	for _, c := range []Config{
		{ID: "m", Type: "email", SMTP: "mail.test", PasswordFile: pw, From: "a@b.test", To: []string{"c@d.test"}},
		{ID: "m", Type: "email", SMTP: "mail.test:587", PasswordFile: pw, From: "nope", To: []string{"c@d.test"}},
		{ID: "m", Type: "email", SMTP: "mail.test:587", PasswordFile: pw, From: "a@b.test"},
		{ID: "m", Type: "email", SMTP: "mail.test:587", PasswordFile: "/nonexistent", From: "a@b.test", To: []string{"c@d.test"}},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	ch, err := New(Config{ID: "m", Type: "email", SMTP: "mail.test:465", PasswordFile: pw, From: "a@b.test", To: []string{"c@d.test"}})
	if err != nil || !ch.(*email).implicit {
		t.Fatalf("465 is implicit TLS: %v", err)
	}
}

func TestEmailStandIn(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pw, []byte("s3cret-pass"), 0o600)
	for _, smtpAddr := range []string{"mail.test:587", "192.0.2.1:25"} {
		if _, err := New(Config{ID: "m", Type: "email", SMTP: smtpAddr, StandIn: true, PasswordFile: pw, From: "a@b.test", To: []string{"c@d.test"}}); err == nil {
			t.Errorf("stand_in accepted %s", smtpAddr)
		}
	}
	cert, _ := testCert(t)
	f := startSMTP(t, cert, false, false) // no TLS at all, on loopback
	ch, err := New(Config{ID: "m", Type: "email", SMTP: f.addr, StandIn: true, PasswordFile: pw, From: "obs@example.test", To: []string{"ops@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if msg := <-f.got; !strings.Contains(msg, "mail.example.org SMTP (25) is down") {
		t.Fatalf("message: %s", msg)
	}
}
