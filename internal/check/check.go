// Package check runs Cairn OBS's active checks (web, TCP and banners, TLS,
// DNS, ping) from an agent. obs-beacond hands each agent its list; the agent
// runs every check on its own schedule and sends the latest results with
// its next report. Judging (two hosts must agree) happens in obs-beacond.
package check

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Spec is one check, as written in checks.yaml and sent to agents. Exactly
// one of HTTP, TCP, DNS and Ping is set.
type Spec struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`

	HTTP string `yaml:"http" json:"http,omitempty"` // URL; redirects are not followed
	TCP  string `yaml:"tcp" json:"tcp,omitempty"`   // host:port
	DNS  string `yaml:"dns" json:"dns,omitempty"`   // name to resolve
	Ping string `yaml:"ping" json:"ping,omitempty"` // host or address
	RBL  string `yaml:"rbl" json:"rbl,omitempty"`   // an IPv4 address to look up on block lists

	Lists []string `yaml:"lists" json:"lists,omitempty"` // rbl: the lists (DNS zones); default DefaultLists

	Status   int    `yaml:"status" json:"status,omitempty"`     // http: expected status; default 200
	Contains string `yaml:"contains" json:"contains,omitempty"` // http body, tcp banner or dns answer must contain this
	TLS      bool   `yaml:"tls" json:"tls,omitempty"`           // tcp: TLS from the first byte (465, 993)
	Type     string `yaml:"type" json:"type,omitempty"`         // dns: A, AAAA, MX, TXT, CNAME or NS; default A
	Server   string `yaml:"server" json:"server,omitempty"`     // dns: resolver as ip:port; default the host's

	Every   time.Duration `yaml:"every" json:"every"`     // default 60s, at least 30s
	Timeout time.Duration `yaml:"timeout" json:"timeout"` // default 10s

	// Judged by obs-beacond, not sent to agents.
	From            []string `yaml:"from" json:"-"`              // hosts that run it; default every agent
	Severity        string   `yaml:"severity" json:"-"`          // when down: critical (default) or warning
	TLSWarnDays     int      `yaml:"tls_warn_days" json:"-"`     // default 21
	TLSCriticalDays int      `yaml:"tls_critical_days" json:"-"` // default 7
}

// Kind is http, tcp, dns or ping.
func (s Spec) Kind() string {
	switch {
	case s.HTTP != "":
		return "http"
	case s.TCP != "":
		return "tcp"
	case s.DNS != "":
		return "dns"
	case s.Ping != "":
		return "ping"
	case s.RBL != "":
		return "rbl"
	}
	return ""
}

// Target is what the check looks at.
func (s Spec) Target() string {
	switch s.Kind() {
	case "http":
		return s.HTTP
	case "tcp":
		return s.TCP
	case "dns":
		return s.Type + " " + s.DNS
	case "rbl":
		return s.RBL + " on " + strings.Join(s.Lists, ", ")
	}
	return s.Ping
}

// Check fills in defaults and refuses a spec that can't run.
func (s *Spec) Check() error {
	if s.ID == "" {
		return errors.New("a check without an id")
	}
	n := 0
	for _, v := range []string{s.HTTP, s.TCP, s.DNS, s.Ping, s.RBL} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("check %s: set exactly one of http, tcp, dns, ping and rbl", s.ID)
	}
	if s.RBL != "" && s.Every == 0 {
		s.Every = 30 * time.Minute // block lists don't change fast, and don't like being asked often
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	if s.Every == 0 {
		s.Every = time.Minute
	}
	if s.Every < 30*time.Second {
		return fmt.Errorf("check %s: every must be at least 30s", s.ID)
	}
	if s.Timeout == 0 {
		s.Timeout = 10 * time.Second
	}
	if s.Timeout >= s.Every {
		return fmt.Errorf("check %s: timeout must be shorter than every", s.ID)
	}
	if s.Severity == "" {
		s.Severity = "critical"
	}
	if s.Severity != "critical" && s.Severity != "warning" {
		return fmt.Errorf("check %s: severity is critical or warning", s.ID)
	}
	if s.TLSWarnDays == 0 {
		s.TLSWarnDays = 21
	}
	if s.TLSCriticalDays == 0 {
		s.TLSCriticalDays = 7
	}
	switch s.Kind() {
	case "http":
		u, err := url.Parse(s.HTTP)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("check %s: http needs an http(s) URL", s.ID)
		}
		if s.Status == 0 {
			s.Status = 200
		}
	case "tcp":
		if _, _, err := net.SplitHostPort(s.TCP); err != nil {
			return fmt.Errorf("check %s: tcp needs host:port", s.ID)
		}
	case "rbl":
		if ip := net.ParseIP(s.RBL); ip == nil || ip.To4() == nil {
			return fmt.Errorf("check %s: rbl needs an IPv4 address", s.ID)
		}
		if len(s.Lists) == 0 {
			s.Lists = DefaultLists
		}
	case "dns":
		if s.Type == "" {
			s.Type = "A"
		}
		s.Type = strings.ToUpper(s.Type)
		switch s.Type {
		case "A", "AAAA", "MX", "TXT", "CNAME", "NS":
		default:
			return fmt.Errorf("check %s: dns type %s isn't supported", s.ID, s.Type)
		}
	}
	return nil
}

// Run runs one check once.
func Run(ctx context.Context, s Spec) report.CheckResult {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	start := time.Now()
	r := report.CheckResult{ID: s.ID, At: start.UTC()}
	var err error
	switch s.Kind() {
	case "http":
		err = runHTTP(ctx, s, &r)
	case "tcp":
		err = runTCP(ctx, s, &r)
	case "dns":
		err = runDNS(ctx, s, &r)
	case "ping":
		err = runPing(ctx, s, &r)
	case "rbl":
		err = runRBL(ctx, s, &r)
	default:
		err = errors.New("unknown kind of check")
	}
	if r.Ms == 0 {
		r.Ms = float64(time.Since(start).Microseconds()) / 1000
	}
	r.OK = err == nil
	if err != nil {
		r.Error = plain(err)
	}
	return r
}

// plain trims Go's error wrapping to what a person needs.
func plain(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	// "dial tcp 192.0.2.1:25: connect: connection refused" → "connection refused"
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
		var se *os.SyscallError
		if errors.As(err, &se) {
			err = se.Err
		}
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func runHTTP(ctx context.Context, s Spec, r *report.CheckResult) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.HTTP, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "obs-beacon-check/"+version.Version)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true},
		// A redirect is an answer, not something to follow: checks name the URL they mean.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r.Status = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.CertExpires = resp.TLS.PeerCertificates[0].NotAfter.UTC()
		r.Cert = certinfo.Of(resp.TLS.PeerCertificates[0])
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil && s.Contains != "" {
		return fmt.Errorf("reading the page: %w", err)
	}
	if resp.StatusCode != s.Status {
		return fmt.Errorf("HTTP %d, expected %d", resp.StatusCode, s.Status)
	}
	if s.Contains != "" && !strings.Contains(string(body), s.Contains) {
		return fmt.Errorf("the page doesn't contain %q", s.Contains)
	}
	return nil
}

func runTCP(ctx context.Context, s Spec, r *report.CheckResult) error {
	host, _, _ := net.SplitHostPort(s.TCP)
	var d net.Dialer
	var conn net.Conn
	var err error
	if s.TLS {
		td := tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
		conn, err = td.DialContext(ctx, "tcp", s.TCP)
	} else {
		conn, err = d.DialContext(ctx, "tcp", s.TCP)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if tc, ok := conn.(*tls.Conn); ok {
		if certs := tc.ConnectionState().PeerCertificates; len(certs) > 0 {
			r.CertExpires = certs[0].NotAfter.UTC()
			r.Cert = certinfo.Of(certs[0])
		}
	}
	if s.Contains == "" {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, 1024), 1024).ReadString('\n')
	r.Answer = short(line)
	if err != nil && line == "" {
		return fmt.Errorf("no greeting: %s", plain(err))
	}
	// Say goodbye properly, so mail servers don't log a dropped client.
	switch {
	case strings.HasPrefix(line, "220"):
		_, _ = conn.Write([]byte("QUIT\r\n"))
	case strings.HasPrefix(line, "* OK"):
		_, _ = conn.Write([]byte("a LOGOUT\r\n"))
	}
	if !strings.Contains(line, s.Contains) {
		return fmt.Errorf("greeting %q doesn't contain %q", r.Answer, s.Contains)
	}
	return nil
}

func runDNS(ctx context.Context, s Spec, r *report.CheckResult) error {
	res := net.DefaultResolver
	if s.Server != "" {
		res = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, s.Server)
		}}
	}
	var answers []string
	var err error
	switch s.Type {
	case "A", "AAAA":
		netw := "ip4"
		if s.Type == "AAAA" {
			netw = "ip6"
		}
		var ips []net.IP
		if ips, err = res.LookupIP(ctx, netw, s.DNS); err == nil {
			for _, ip := range ips {
				answers = append(answers, ip.String())
			}
		}
	case "MX":
		var mx []*net.MX
		if mx, err = res.LookupMX(ctx, s.DNS); err == nil {
			for _, m := range mx {
				answers = append(answers, fmt.Sprintf("%d %s", m.Pref, m.Host))
			}
		}
	case "TXT":
		answers, err = res.LookupTXT(ctx, s.DNS)
	case "CNAME":
		var c string
		if c, err = res.LookupCNAME(ctx, s.DNS); err == nil {
			answers = []string{c}
		}
	case "NS":
		var ns []*net.NS
		if ns, err = res.LookupNS(ctx, s.DNS); err == nil {
			for _, n := range ns {
				answers = append(answers, n.Host)
			}
		}
	}
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return fmt.Errorf("no %s record for %s", s.Type, s.DNS)
		}
		return err
	}
	all := strings.Join(answers, ", ")
	r.Answer = short(all)
	if len(answers) == 0 {
		return fmt.Errorf("no %s record for %s", s.Type, s.DNS)
	}
	if s.Contains != "" && !strings.Contains(all, s.Contains) {
		return fmt.Errorf("answer %q doesn't contain %q", r.Answer, s.Contains)
	}
	return nil
}
