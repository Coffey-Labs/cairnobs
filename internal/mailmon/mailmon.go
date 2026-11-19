// Package mailmon watches mail the way a person uses it, with a monitor
// account of its own: it signs in over SMTP submission, IMAP and JMAP,
// sends itself a message and times how long it takes to arrive, and reads
// the size of the outgoing queue. It runs in obs-beacond, where the account's
// password lives; obs-beacond turns what it finds into conditions.
package mailmon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config is the `mail` section of obs-beacond.yaml.
type Config struct {
	Account      string `yaml:"account"`       // the monitor account, e.g. monitor@example.org
	PasswordFile string `yaml:"password_file"` // its password, re-read on every round
	Server       string `yaml:"server"`        // the mail server's name, e.g. mail.example.org
	SMTP         string `yaml:"smtp"`          // submission host:port; default server:587 (STARTTLS)
	IMAP         string `yaml:"imap"`          // host:port; default server:993 (TLS)
	URL          string `yaml:"url"`           // JMAP and sign-in; default https://server
	// JMAP signs in the way webmail does: with a registered OAuth client
	// (PKCE, no secret) and its redirect URL, which is never visited.
	ClientID    string `yaml:"client_id"`
	RedirectURL string `yaml:"redirect_url"`
	// The agent host that serves the mail server, so its maintenance window
	// holds these alerts too.
	Host string `yaml:"host"`

	Every         time.Duration `yaml:"every"`           // default 5m
	Wait          time.Duration `yaml:"wait"`            // how long a test message may take; default 4m, less than every
	RoundTripWarn time.Duration `yaml:"round_trip_warn"` // slower than this warns; default 1m
	QueueWarn     int           `yaml:"queue_warn"`      // messages waiting; default 50
	QueueCritical int           `yaml:"queue_critical"`  // default 500

	// StandIn: the mail server is a stand-in on this machine, such as the
	// fleet simulator's demo (scripts/fleet): SMTP and IMAP without TLS,
	// and smtp, imap and url must all be on loopback.
	StandIn bool `yaml:"stand_in"`

	PlainIMAP bool `yaml:"-"` // set by StandIn
	PlainSMTP bool `yaml:"-"`
}

// Defaults fills in what was left out and refuses what can't work.
func (c *Config) Defaults() error {
	if c.Account == "" || c.PasswordFile == "" || c.Server == "" {
		return errors.New("mail: account, password_file and server are required")
	}
	if c.ClientID == "" || c.RedirectURL == "" {
		return errors.New("mail: client_id and redirect_url are required (JMAP signs in as webmail does)")
	}
	if c.SMTP == "" {
		c.SMTP = net.JoinHostPort(c.Server, "587")
	}
	if c.IMAP == "" {
		c.IMAP = net.JoinHostPort(c.Server, "993")
	}
	if c.URL == "" {
		c.URL = "https://" + c.Server
	}
	c.URL = strings.TrimRight(c.URL, "/")
	if c.StandIn {
		u, err := url.Parse(c.URL)
		if err != nil || !loopback(u.Hostname()) || !loopbackAddr(c.SMTP) || !loopbackAddr(c.IMAP) {
			return errors.New("mail: stand_in needs smtp, imap and url on loopback (a stand-in on this machine)")
		}
		c.PlainIMAP, c.PlainSMTP = true, true
	}
	if c.Every == 0 {
		c.Every = 5 * time.Minute
	}
	if c.Wait == 0 {
		c.Wait = 4 * time.Minute
	}
	if c.Wait >= c.Every {
		return errors.New("mail: wait must be shorter than every")
	}
	if c.RoundTripWarn == 0 {
		c.RoundTripWarn = time.Minute
	}
	if c.QueueWarn == 0 {
		c.QueueWarn = 50
	}
	if c.QueueCritical == 0 {
		c.QueueCritical = 500
	}
	return nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func loopbackAddr(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	return err == nil && loopback(h)
}

// Snapshot is the latest round. Fails count rounds in a row that went
// wrong, so one bad round (a reboot, a blip) can be told from an outage.
type Snapshot struct {
	Checked time.Time   `json:"checked"`
	Error   string      `json:"error,omitempty"` // the round couldn't start (no password)
	SignIns []SignIn    `json:"signins"`         // SMTP, IMAP, JMAP
	Trip    Trip        `json:"trip"`
	History []TripPoint `json:"history"` // the last day, oldest first
	Queue   Queue       `json:"queue"`
}

// SignIn is one protocol's sign-in.
type SignIn struct {
	Name  string  `json:"name"`
	OK    bool    `json:"ok"`
	Ms    float64 `json:"ms"`
	Error string  `json:"error,omitempty"`
	Fails int     `json:"fails"`
}

// Trip is the latest send-and-receive.
type Trip struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Seconds float64   `json:"seconds"`
	Error   string    `json:"error,omitempty"`
	Fails   int       `json:"fails"`
}

// TripPoint is one round on the chart; Seconds is -1 when nothing arrived.
type TripPoint struct {
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
}

// Queue is the outgoing queue, server-wide.
type Queue struct {
	Read   bool       `json:"read"`
	Total  int        `json:"total"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Error  string     `json:"error,omitempty"`
	Fails  int        `json:"fails"`
}

// Watcher runs a round every cfg.Every and keeps the latest Snapshot.
type Watcher struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
	snap   Snapshot
}

// New checks the config and that the password file is readable.
func New(cfg Config) (*Watcher, error) {
	if err := cfg.Defaults(); err != nil {
		return nil, err
	}
	if _, err := readPassword(cfg.PasswordFile); err != nil {
		return nil, err
	}
	return &Watcher{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Config returns the settings in use (defaults filled in).
func (w *Watcher) Config() Config { return w.cfg }

// Snapshot returns the latest round.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.snap
	s.SignIns = append([]SignIn{}, s.SignIns...)
	s.History = append([]TripPoint{}, s.History...)
	return s
}

// Set replaces the snapshot; tests use it to judge a known state.
func (w *Watcher) Set(s Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = s
}

// Run runs a round now and then every cfg.Every until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Every)
	defer t.Stop()
	for {
		w.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func readPassword(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("mail monitor password: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("mail monitor password %s is empty", p)
	}
	return s, nil
}

// Mail servers often limit how many connections one address may open in a
// second (inbuxa's inbound throttle allows 5). A round opens several: the
// SMTP, IMAP and JMAP sign-ins, then the send. So they are spaced stepGap
// apart, and a connection the server drops (EOF, reset, a 4xx reply) is
// tried once more after retryWait before it counts as a failure. A real
// outage still fails both tries. Variables so tests can shorten them.
var (
	stepGap   = time.Second
	retryWait = 3 * time.Second
)

// transient says whether err looks like a connection the server dropped or
// put off, worth one more try, rather than an answer (wrong password, 5xx,
// refused port) or a timeout, which already took its full time.
func transient(err error) bool {
	if err == nil {
		return false
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code >= 400 && te.Code < 500
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// pause waits d, or less if ctx ends; it says whether the whole wait passed.
func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// once runs f with its own timeout.
func once(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(c)
}

// retried runs f, and once more after retryWait if the first try failed
// transiently.
func retried(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	err := once(ctx, d, f)
	if transient(err) && pause(ctx, retryWait) {
		err = once(ctx, d, f)
	}
	return err
}

// Round signs in everywhere, sends a test message and waits for it, and
// reads the queue.
func (w *Watcher) Round(ctx context.Context) {
	start := time.Now()
	prev := w.Snapshot()
	s := Snapshot{Checked: start, History: prev.History}
	pw, err := readPassword(w.cfg.PasswordFile)
	if err != nil {
		s.Error = err.Error()
		s.SignIns, s.Trip, s.Queue = prev.SignIns, prev.Trip, prev.Queue
		w.Set(s)
		return
	}
	fails := map[string]int{}
	for _, si := range prev.SignIns {
		fails[si.Name] = si.Fails
	}
	step := func(name string, f func(context.Context) error) SignIn {
		t := time.Now()
		err := retried(ctx, 20*time.Second, f)
		si := SignIn{Name: name, OK: err == nil, Ms: float64(time.Since(t).Microseconds()) / 1000}
		if err != nil {
			si.Error, si.Fails = plain(err), fails[name]+1
		}
		return si
	}

	var j *jmapSession
	s.SignIns = []SignIn{step("SMTP", func(c context.Context) error { return w.smtpSignIn(c, pw) })}
	pause(ctx, stepGap)
	s.SignIns = append(s.SignIns, step("IMAP", func(c context.Context) error { return w.imapSignIn(c, pw) }))
	pause(ctx, stepGap)
	s.SignIns = append(s.SignIns, step("JMAP", func(c context.Context) (err error) { j, err = w.jmapSignIn(c, pw); return }))

	// The queue, then the round trip (which takes longest).
	if j == nil {
		s.Queue = Queue{Error: "JMAP sign-in failed", Fails: prev.Queue.Fails + 1}
	} else {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		s.Queue = w.queue(c, j)
		cancel()
		if s.Queue.Error != "" {
			s.Queue.Fails = prev.Queue.Fails + 1
		}
	}
	s.Trip = w.roundTrip(ctx, pw, j)
	if !s.Trip.OK {
		s.Trip.Fails = prev.Trip.Fails + 1
	}
	p := TripPoint{At: s.Trip.At, Seconds: -1}
	if s.Trip.OK {
		p.Seconds = s.Trip.Seconds
	}
	s.History = append(s.History, p)
	for len(s.History) > 0 && start.Sub(s.History[0].At) > 24*time.Hour {
		s.History = s.History[1:]
	}
	w.Set(s)
}

// --- SMTP ---

func (w *Watcher) smtpClient(ctx context.Context, pw string) (*smtp.Client, error) {
	host, _, _ := net.SplitHostPort(w.cfg.SMTP)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", w.cfg.SMTP)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !w.cfg.PlainSMTP {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if err := c.Auth(smtp.PlainAuth("", w.cfg.Account, pw, host)); err != nil {
		c.Close()
		return nil, fmt.Errorf("sign-in refused: %w", err)
	}
	return c, nil
}

func (w *Watcher) smtpSignIn(ctx context.Context, pw string) error {
	c, err := w.smtpClient(ctx, pw)
	if err != nil {
		return err
	}
	return c.Quit()
}

func (w *Watcher) send(ctx context.Context, pw, token string, now time.Time) error {
	c, err := w.smtpClient(ctx, pw)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(w.cfg.Account); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := c.Rcpt(w.cfg.Account); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	_, domain, _ := strings.Cut(w.cfg.Account, "@")
	fmt.Fprintf(wc, "From: Cairn OBS monitor <%s>\r\nTo: <%s>\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n"+
		"Auto-Submitted: auto-generated\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
		"Cairn OBS's mail round trip. It deletes this message when it arrives.\r\n",
		w.cfg.Account, w.cfg.Account, subject(token), now.UTC().Format(time.RFC1123Z), token, domain)
	if err := wc.Close(); err != nil {
		return fmt.Errorf("message refused: %w", err)
	}
	return c.Quit()
}

func subject(token string) string { return "Cairn OBS round trip " + token }

// --- IMAP ---

// imapSignIn logs in and out. It speaks just enough IMAP for that.
func (w *Watcher) imapSignIn(ctx context.Context, pw string) error {
	host, _, _ := net.SplitHostPort(w.cfg.IMAP)
	var d net.Dialer
	var conn net.Conn
	var err error
	if w.cfg.PlainIMAP {
		conn, err = d.DialContext(ctx, "tcp", w.cfg.IMAP)
	} else {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: host}}).DialContext(ctx, "tcp", w.cfg.IMAP)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("no greeting: %w", err)
	}
	if !strings.HasPrefix(line, "* OK") {
		return fmt.Errorf("greeting: %s", strings.TrimSpace(line))
	}
	cmd := func(tag, text string) error {
		if _, err := io.WriteString(conn, tag+" "+text+"\r\n"); err != nil {
			return err
		}
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return err
			}
			if rest, ok := strings.CutPrefix(line, tag+" "); ok {
				if strings.HasPrefix(rest, "OK") {
					return nil
				}
				return errors.New(strings.TrimSpace(rest))
			}
		}
	}
	if err := cmd("b1", "LOGIN "+quote(w.cfg.Account)+" "+quote(pw)); err != nil {
		return fmt.Errorf("sign-in refused: %w", err)
	}
	return cmd("b2", "LOGOUT")
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// --- JMAP ---

type jmapSession struct {
	token, api, mail, registry string
	using                      []string
}

// jmapSignIn signs in as webmail does: the account's password for a code,
// the code (with PKCE) for a token, then the JMAP session.
func (w *Watcher) jmapSignIn(ctx context.Context, pw string) (*jmapSession, error) {
	verifier := randomToken(32)
	sum := sha256.Sum256([]byte(verifier))
	var login struct {
		Type        string `json:"type"`
		ClientCode  string `json:"client_code"`
		ClientCode2 string `json:"clientCode"`
	}
	body, _ := json.Marshal(map[string]string{"type": "authCode", "accountName": w.cfg.Account, "accountSecret": pw,
		"clientId": w.cfg.ClientID, "redirectUri": w.cfg.RedirectURL, "scope": "openid",
		"codeChallenge": base64.RawURLEncoding.EncodeToString(sum[:]), "codeChallengeMethod": "S256"})
	if err := w.do(ctx, http.MethodPost, w.cfg.URL+"/api/auth", "application/json", bytes.NewReader(body), "", &login); err != nil {
		return nil, err
	}
	code := login.ClientCode + login.ClientCode2
	if login.Type != "authenticated" || code == "" {
		if login.Type == "" {
			login.Type = "no answer"
		}
		return nil, fmt.Errorf("sign-in refused (%s)", login.Type)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {w.cfg.RedirectURL},
		"client_id": {w.cfg.ClientID}, "code_verifier": {verifier}}
	if err := w.do(ctx, http.MethodPost, w.cfg.URL+"/auth/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), "", &tok); err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token: none given")
	}
	var sess struct {
		APIURL          string            `json:"apiUrl"`
		Capabilities    map[string]any    `json:"capabilities"`
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := w.do(ctx, http.MethodGet, w.cfg.URL+"/jmap/session", "", nil, tok.AccessToken, &sess); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	j := &jmapSession{token: tok.AccessToken, api: sess.APIURL, mail: sess.PrimaryAccounts["urn:ietf:params:jmap:mail"]}
	if j.api == "" || j.mail == "" {
		return nil, errors.New("session: no mail account")
	}
	if u, err := url.Parse(j.api); err == nil && !u.IsAbs() {
		j.api = w.cfg.URL + j.api
	}
	j.registry = sess.PrimaryAccounts["urn:inbuxa:jmap"]
	if j.registry == "" {
		j.registry = j.mail
	}
	for c := range sess.Capabilities {
		j.using = append(j.using, c)
	}
	return j, nil
}

func (w *Watcher) do(ctx context.Context, method, u, ctype string, body io.Reader, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unreadable answer: %w", err)
	}
	return nil
}

// call makes JMAP method calls and returns each answer's arguments, in
// order. A method error comes back as an error.
func (w *Watcher) call(ctx context.Context, j *jmapSession, calls ...[]any) ([]json.RawMessage, error) {
	for i := range calls {
		calls[i] = append(calls[i], fmt.Sprint(i))
	}
	body, _ := json.Marshal(map[string]any{"using": j.using, "methodCalls": calls})
	var res struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := w.do(ctx, http.MethodPost, j.api, "application/json", bytes.NewReader(body), j.token, &res); err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(res.MethodResponses))
	for _, m := range res.MethodResponses {
		if len(m) < 2 {
			return nil, errors.New("malformed answer")
		}
		var name string
		json.Unmarshal(m[0], &name)
		if name == "error" {
			var e struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}
			json.Unmarshal(m[1], &e)
			return nil, fmt.Errorf("%s %s", e.Type, e.Description)
		}
		out = append(out, m[1])
	}
	if len(out) != len(calls) {
		return nil, errors.New("missing answers")
	}
	return out, nil
}

func (w *Watcher) queue(ctx context.Context, j *jmapSession) Queue {
	res, err := w.call(ctx, j,
		[]any{"x:QueuedMessage/query", map[string]any{"accountId": j.registry, "calculateTotal": true, "limit": 200}},
		[]any{"x:QueuedMessage/get", map[string]any{"accountId": j.registry, "properties": []string{"createdAt"},
			"#ids": map[string]any{"resultOf": "0", "name": "x:QueuedMessage/query", "path": "/ids"}}})
	if err != nil {
		return Queue{Error: "queue: " + err.Error()}
	}
	var q struct {
		Total int `json:"total"`
	}
	var g struct {
		List []struct {
			CreatedAt time.Time `json:"createdAt"`
		} `json:"list"`
	}
	if json.Unmarshal(res[0], &q) != nil || json.Unmarshal(res[1], &g) != nil {
		return Queue{Error: "queue: unreadable answer"}
	}
	out := Queue{Read: true, Total: q.Total}
	for _, m := range g.List {
		if t := m.CreatedAt; !t.IsZero() && (out.Oldest == nil || t.Before(*out.Oldest)) {
			out.Oldest = &t
		}
	}
	return out
}

// roundTrip sends a message to the monitor account and waits for it over
// JMAP. The mailbox is the monitor's own: anything in it over an hour old
// (earlier test messages that came late, bounces) is deleted first.
func (w *Watcher) roundTrip(ctx context.Context, pw string, j *jmapSession) Trip {
	start := time.Now()
	t := Trip{At: start}
	if j == nil {
		t.Error = "JMAP sign-in failed, so nothing can be checked for arrival"
		return t
	}
	c, cancel := context.WithTimeout(ctx, w.cfg.Wait+30*time.Second)
	defer cancel()
	w.sweep(c, j, start.Add(-time.Hour))
	token := randomToken(9)
	pause(c, stepGap)
	// A retry after the server dropped the end of DATA may deliver the
	// message twice; find takes either, and the sweep removes the other.
	err := retried(c, 30*time.Second, func(sc context.Context) error { return w.send(sc, pw, token, start) })
	if err != nil {
		t.Error = "sending: " + plain(err)
		return t
	}
	sent := time.Now()
	deadline := sent.Add(w.cfg.Wait)
	pause := 500 * time.Millisecond
	for {
		id, err := w.find(c, j, token, start)
		if err == nil && id != "" {
			t.OK, t.Seconds = true, time.Since(sent).Seconds()
			w.call(c, j, []any{"Email/set", map[string]any{"accountId": j.mail, "destroy": []string{id}}})
			return t
		}
		if time.Now().Add(pause).After(deadline) {
			t.Error = fmt.Sprintf("sent, but it hadn't arrived after %s", w.cfg.Wait)
			if err != nil {
				t.Error += " (last look: " + plain(err) + ")"
			}
			return t
		}
		select {
		case <-c.Done():
			t.Error = "stopped waiting"
			return t
		case <-time.After(pause):
		}
		if pause < 5*time.Second {
			pause *= 2
		}
	}
}

// find looks for the message by its subject among what arrived since the
// round began. Received date, not full-text search: the search index can
// lag behind delivery.
func (w *Watcher) find(ctx context.Context, j *jmapSession, token string, since time.Time) (string, error) {
	res, err := w.call(ctx, j,
		[]any{"Email/query", map[string]any{"accountId": j.mail, "limit": 50,
			"filter": map[string]any{"after": since.Add(-time.Minute).UTC().Format(time.RFC3339)},
			"sort":   []map[string]any{{"property": "receivedAt", "isAscending": false}}}},
		[]any{"Email/get", map[string]any{"accountId": j.mail, "properties": []string{"subject"},
			"#ids": map[string]any{"resultOf": "0", "name": "Email/query", "path": "/ids"}}})
	if err != nil {
		return "", err
	}
	var g struct {
		List []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"list"`
	}
	if err := json.Unmarshal(res[1], &g); err != nil {
		return "", err
	}
	for _, m := range g.List {
		if m.Subject == subject(token) {
			return m.ID, nil
		}
	}
	return "", nil
}

func (w *Watcher) sweep(ctx context.Context, j *jmapSession, before time.Time) {
	res, err := w.call(ctx, j, []any{"Email/query", map[string]any{"accountId": j.mail, "limit": 200,
		"filter": map[string]any{"before": before.UTC().Format(time.RFC3339)}}})
	if err != nil {
		return
	}
	var q struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(res[0], &q) == nil && len(q.IDs) > 0 {
		w.call(ctx, j, []any{"Email/set", map[string]any{"accountId": j.mail, "destroy": q.IDs}})
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// plain trims an error to what a person needs, and never echoes the
// password (net/smtp and IMAP servers don't, but be sure).
func plain(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": dial tcp "); i >= 0 {
		if j := strings.LastIndex(s, ": "); j > i {
			s = s[:i] + ": " + s[j+2:]
		}
	}
	return s
}
