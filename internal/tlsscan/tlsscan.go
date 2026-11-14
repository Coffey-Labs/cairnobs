// Package tlsscan finds the certificates a host serves (docs/cert-inventory.md,
// step 2): the agent tries a TLS handshake on each TCP port the host
// listens on, once a day, and keeps the certificate it's served. Mail,
// LDAP and PostgreSQL ports are asked for STARTTLS first. Nothing is
// configured: the ports come from the map sample. Only public
// certificates are seen; nothing is sent but the protocol's greeting.
package tlsscan

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

const (
	// Every is how often a port is tried again.
	Every = 24 * time.Hour
	// resend is how often the list goes to obs-beacond when nothing
	// changed, so a new copy of obs-beacond has it within the hour.
	resend = 30 * time.Minute
	// perRound is how many ports one round tries; rounds come every
	// RoundEvery.
	perRound   = 5
	RoundEvery = 10 * time.Second
	timeout    = 3 * time.Second
)

// skip are ports never tried: they don't speak TLS, and a ClientHello
// only puts a line in their log.
var skip = map[int]bool{22: true, 53: true, 111: true, 123: true, 2049: true, 5355: true}

// starttls are the ports asked to start TLS, by protocol.
var starttls = map[int]string{25: "smtp", 587: "smtp", 143: "imap", 110: "pop3", 389: "ldap", 5432: "postgres"}

type target struct {
	addr string // what's dialed
	port int
	name string // sent as SNI: a cluster's Ingress and Route hosts
}

type entry struct {
	owner    string
	next     time.Time
	seen     *report.CertSeen // nil: no TLS there
	caSigned bool             // a CA-signed certificate was served here
}

// Scanner keeps each port's certificate. Update and Report come from the
// agent's tick; Round from its own goroutine.
type Scanner struct {
	host string
	mu   sync.Mutex
	tgts map[target]*entry
	sent time.Time
	last string // what was sent, to send again only on a change
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	ask  map[int]string // ports asked for STARTTLS: starttls, or the tests'
}

// New is a scanner for host (its name spreads the day's tries).
func New(host string) *Scanner {
	var d net.Dialer
	return &Scanner{host: host, tgts: map[target]*entry{}, dial: d.DialContext, ask: starttls}
}

// Update sets the ports the host listens on now, from a map sample.
func (s *Scanner) Update(listen []report.Listener, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[target]bool{}
	for _, l := range listen {
		if l.Proto != "tcp" || skip[l.Port] || l.Port <= 0 {
			continue
		}
		t := target{addr: dialAddr(l.Addr), port: l.Port}
		keep[t] = true
		if e := s.tgts[t]; e != nil {
			e.owner = l.Owner
			continue
		}
		s.tgts[t] = &entry{owner: l.Owner, next: now} // new: tried soon
	}
	for t := range s.tgts {
		if !keep[t] {
			delete(s.tgts, t)
		}
	}
}

// UpdateNames sets the names to try instead, on 443 with each as SNI, as
// an outsider would: a cluster agent's Ingress and Route hosts (step 4).
func (s *Scanner) UpdateNames(names []string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[target]bool{}
	for _, n := range names {
		if n == "" || strings.HasPrefix(n, "*") {
			continue // a wildcard names no one host
		}
		t := target{addr: n, port: 443, name: n}
		keep[t] = true
		if s.tgts[t] == nil {
			s.tgts[t] = &entry{next: now}
		}
	}
	for t := range s.tgts {
		if !keep[t] {
			delete(s.tgts, t)
		}
	}
}

// Named is what each name served the last time it was tried, by name.
func (s *Scanner) Named() map[string]report.CertSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]report.CertSeen{}
	for t, e := range s.tgts {
		if t.name != "" && e.seen != nil {
			out[t.name] = *e.seen
		}
	}
	return out
}

// dialAddr is the address to reach a listener on: loopback for every
// address, else its own.
func dialAddr(a string) string {
	switch a {
	case "*", "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	}
	return strings.Trim(a, "[]")
}

// Round tries the ports that are due, perRound at most.
func (s *Scanner) Round(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var due []target
	for t, e := range s.tgts {
		if !now.Before(e.next) {
			due = append(due, t)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(due, func(a, b target) int {
		if a.port != b.port {
			return a.port - b.port
		}
		return strings.Compare(a.addr, b.addr)
	})
	if len(due) > perRound {
		due = due[:perRound]
	}
	for _, t := range due {
		seen, self := s.try(ctx, t, now)
		s.mu.Lock()
		if e := s.tgts[t]; e != nil {
			e.seen = seen
			e.next = now.Add(Every + s.jitter(t))
			if seen != nil {
				seen.Owner = e.owner
				if self && e.caSigned {
					seen.Wrong = append(seen.Wrong, "self-signed where a CA-signed certificate was")
				}
				e.caSigned = e.caSigned || !self
			}
		}
		s.mu.Unlock()
	}
}

// TryNow handshakes addr:port now, out of turn (after a reload), and keeps
// what it found for the next report; nil when it doesn't speak TLS. Only
// a port the host listens on is tried: false for any other.
func (s *Scanner) TryNow(ctx context.Context, addr string, port int, now time.Time) (*report.CertSeen, bool) {
	t := target{addr: addr, port: port}
	s.mu.Lock()
	_, known := s.tgts[t]
	s.mu.Unlock()
	if !known {
		return nil, false
	}
	seen, self := s.try(ctx, t, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.tgts[t]; e != nil {
		e.seen = seen
		if seen != nil {
			seen.Owner = e.owner
			if self && e.caSigned {
				seen.Wrong = append(seen.Wrong, "self-signed where a CA-signed certificate was")
			}
			e.caSigned = e.caSigned || !self
		}
		s.last = "" // send it with the next report
	}
	return seen, true
}

// jitter spreads a host's ports over an hour, the same for each port, so
// a fleet doesn't try them all at once.
func (s *Scanner) jitter(t target) time.Duration {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s:%d%s", s.host, t.port, t.name)
	return time.Duration(h.Sum32()%3600) * time.Second
}

// try handshakes one port; nil when it doesn't speak TLS. self says the
// certificate signed itself.
func (s *Scanner) try(ctx context.Context, t target, now time.Time) (seen *report.CertSeen, self bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := s.dial(ctx, "tcp", net.JoinHostPort(t.addr, strconv.Itoa(t.port)))
	if err != nil {
		return nil, false
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	how := "tls"
	if p, ok := s.ask[t.port]; ok {
		if err := startTLS(conn, p); err != nil {
			return nil, false
		}
		how = p
	}
	tc := tls.Client(conn, &tls.Config{ServerName: t.name, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}) // collecting, not trusting
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, false
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, false
	}
	self = selfSigned(certs[0])
	return &report.CertSeen{Addr: t.addr, Port: t.port, How: how, Cert: *certinfo.Of(certs[0]), Chain: len(certs) - 1, At: now.UTC(),
		Wrong: Wrong(certs, t.name, self)}, self
}

func selfSigned(x *x509.Certificate) bool {
	// CheckSignatureFrom refuses a parent that isn't a CA, and a
	// self-signed leaf often isn't marked one.
	return bytes.Equal(x.RawIssuer, x.RawSubject) && x.CheckSignature(x.SignatureAlgorithm, x.RawTBSCertificate, x.Signature) == nil
}

// Wrong is what's wrong with how a chain is served (step 5), from the
// chain alone: no root store is needed. name is the name asked for (SNI),
// "" for none.
//   - A name the leaf doesn't cover.
//   - A CA-signed leaf that says where its issuer is (an AIA URL, as every
//     public CA's do) sent alone: public CAs never sign a leaf from a root,
//     so the intermediate is missing and some clients will fail.
//   - An intermediate sent that didn't sign the leaf.
func Wrong(certs []*x509.Certificate, name string, self bool) []string {
	var out []string
	leaf := certs[0]
	if name != "" && leaf.VerifyHostname(name) != nil {
		out = append(out, "doesn't cover "+name)
	}
	switch {
	case self:
	case len(certs) == 1 && len(leaf.IssuingCertificateURL) > 0:
		out = append(out, "sent without its intermediate certificate")
	case len(certs) > 1 && leaf.CheckSignatureFrom(certs[1]) != nil:
		out = append(out, "sent with an intermediate that didn't sign it")
	}
	return out
}

// Report is the list to send: when it changed, or every resend; nil
// otherwise (obs-beacond keeps the last).
func (s *Scanner) Report(now time.Time) *report.CertScan {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []report.CertSeen{}
	var key strings.Builder
	for _, e := range s.tgts {
		if e.seen != nil {
			list = append(list, *e.seen)
		}
	}
	slices.SortFunc(list, func(a, b report.CertSeen) int {
		if c := strings.Compare(a.Addr, b.Addr); c != 0 {
			return c
		}
		return a.Port - b.Port
	})
	for _, c := range list {
		fmt.Fprintf(&key, "%s:%d %s %s %s|", c.Addr, c.Port, c.Owner, c.Cert.Fingerprint, strings.Join(c.Wrong, ";"))
	}
	if key.String() == s.last && now.Sub(s.sent) < resend {
		return nil
	}
	s.last, s.sent = key.String(), now
	return &report.CertScan{At: now.UTC(), List: list}
}

var errRefused = errors.New("STARTTLS refused")

// startTLS asks a plain-text protocol to start TLS.
func startTLS(conn net.Conn, proto string) error {
	r := bufio.NewReaderSize(io.LimitReader(conn, 64<<10), 4096)
	line := func() (string, error) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err
	}
	// reply reads an SMTP reply, its lines "250-…" then "250 …".
	reply := func() (string, error) {
		for {
			l, err := line()
			if err != nil {
				return "", err
			}
			if len(l) < 4 || l[3] != '-' {
				return l, nil
			}
		}
	}
	send := func(s string) error { _, err := io.WriteString(conn, s); return err }
	switch proto {
	case "smtp":
		if l, err := reply(); err != nil || !strings.HasPrefix(l, "220") {
			return errRefused
		}
		if send("EHLO obs-beacon-agent\r\n") != nil {
			return errRefused
		}
		if l, err := reply(); err != nil || !strings.HasPrefix(l, "250") {
			return errRefused
		}
		if send("STARTTLS\r\n") != nil {
			return errRefused
		}
		if l, err := reply(); err != nil || !strings.HasPrefix(l, "220") {
			return errRefused
		}
	case "imap":
		if l, err := line(); err != nil || !strings.HasPrefix(l, "* OK") {
			return errRefused
		}
		if send("a1 STARTTLS\r\n") != nil {
			return errRefused
		}
		for {
			l, err := line()
			if err != nil {
				return errRefused
			}
			if strings.HasPrefix(l, "a1 ") {
				if !strings.HasPrefix(l, "a1 OK") {
					return errRefused
				}
				break
			}
		}
	case "pop3":
		if l, err := line(); err != nil || !strings.HasPrefix(l, "+OK") {
			return errRefused
		}
		if send("STLS\r\n") != nil {
			return errRefused
		}
		if l, err := line(); err != nil || !strings.HasPrefix(l, "+OK") {
			return errRefused
		}
	case "postgres":
		// SSLRequest: length 8, code 80877103; 'S' says yes.
		if _, err := conn.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
			return errRefused
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(conn, b); err != nil || b[0] != 'S' {
			return errRefused
		}
		return nil // nothing buffered: the server waits for the ClientHello
	case "ldap":
		// An ExtendedRequest for StartTLS (RFC 4511, 4.14), message 1.
		oid := "1.3.6.1.4.1.1466.20037"
		req := append([]byte{0x30, byte(5 + 2 + len(oid)), 0x02, 0x01, 0x01, 0x77, byte(2 + len(oid)), 0x80, byte(len(oid))}, oid...)
		if _, err := conn.Write(req); err != nil {
			return errRefused
		}
		b := make([]byte, 256)
		n, err := conn.Read(b)
		// An ExtendedResponse (0x78) whose resultCode is success (0x0a 0x01 0x00).
		if err != nil || !bytes.Contains(b[:n], []byte{0x78}) || !bytes.Contains(b[:n], []byte{0x0a, 0x01, 0x00}) {
			return errRefused
		}
		return nil
	}
	if r.Buffered() > 0 {
		return errRefused // the server spoke past its "go ahead": not TLS next
	}
	return nil
}
