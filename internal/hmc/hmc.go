// Package hmc reads an IBM Hardware Management Console's Power servers and
// their partitions (IBM i, AIX, Linux and VIOS) through the HMC's REST API
// (docs/ibmi.md, step 4), as a user with the built-in hmcviewer task role:
// it can look, never change. The one call that isn't a GET is the logon
// (a PUT, as the API has it), and the logoff that ends its session.
package hmc

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/tlspin"
)

// Config is one entry in the agent's hmc: list.
type Config struct {
	// Name is what the HMC is kept under, beside the agent's name
	// ("mgmt1:hmc1.example.org"): default its host name.
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`           // https://hmc1.example.org (port 12443 is added)
	Username     string `yaml:"username"`      // an HMC user with the hmcviewer task role
	PasswordFile string `yaml:"password_file"` // its password, alone in the file
	CAFile       string `yaml:"ca_file"`       // the HMC's CA, PEM; else the system's
	// Fingerprint pins the HMC's certificate (SHA-256, hex, colons or not)
	// when it isn't signed by a CA the agent trusts: its self-signed one,
	// as installed.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 60s
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Every == 0 {
		c.Every = time.Minute
	}
	if c.Name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			c.Name = strings.ToLower(u.Hostname())
		}
	}
}

// Client is one HMC, and the session it's logged on with: kept between
// reads, as the HMC limits how many sessions there are, and made again
// when the HMC says it lapsed.
type Client struct {
	base, name string
	user, pass string
	hc         *http.Client

	mu      sync.Mutex
	session string
}

// New checks cfg and reads the password and the CA; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("hmc: url %q: want https://hmc", cfg.URL)
	}
	if u.Port() == "" {
		u.Host += ":12443"
	}
	if cfg.Username == "" {
		return nil, errors.New("hmc: no username")
	}
	b, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return nil, fmt.Errorf("hmc: the password: %w", err)
	}
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("hmc: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	return &Client{base: u.Scheme + "://" + u.Host + "/rest/api", name: u.Hostname(), user: cfg.Username,
		pass: strings.TrimRight(string(b), "\r\n"), hc: &http.Client{Transport: tr, Timeout: time.Minute}}, nil
}

// Name is what the HMC is called here: its host name.
func (c *Client) Name() string { return c.name }

// StatusError is the HMC refusing a call.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("hmc: %d %s", e.Code, e.Msg) }

const webXML = "application/vnd.ibm.powervm.web+xml"

// logon starts a session: a PUT of the user and password, answered with
// the session's token.
func (c *Client) logon(ctx context.Context) (string, error) {
	var body bytes.Buffer
	body.WriteString(`<LogonRequest xmlns="http://www.ibm.com/xmlns/systems/power/firmware/web/mc/2012_10/" schemaVersion="V1_0"><UserID>`)
	xml.EscapeText(&body, []byte(c.user)) //nolint:errcheck // a bytes.Buffer
	body.WriteString(`</UserID><Password>`)
	xml.EscapeText(&body, []byte(c.pass)) //nolint:errcheck // a bytes.Buffer
	body.WriteString(`</Password></LogonRequest>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/web/Logon", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", webXML+"; type=LogonRequest")
	req.Header.Set("Accept", webXML+"; type=LogonResponse")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Code: resp.StatusCode, Msg: "logon: " + apiError(resp)}
	}
	var lr struct {
		Session string `xml:"X-API-Session"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&lr); err != nil || strings.TrimSpace(lr.Session) == "" {
		return "", errors.New("hmc: logon: no session in the answer")
	}
	return strings.TrimSpace(lr.Session), nil
}

// Close ends the session, if there is one.
func (c *Client) Close() {
	c.mu.Lock()
	s := c.session
	c.session = ""
	c.mu.Unlock()
	if s == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/web/Logon", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-API-Session", s)
	if resp, err := c.hc.Do(req); err == nil {
		resp.Body.Close()
	}
}

// get reads path's XML into v, logging on first when there's no session,
// and once more when the HMC says the session lapsed. It says false when
// the HMC answered 204: a feed with nothing in it.
func (c *Client) get(ctx context.Context, path string, v any) (bool, error) {
	for try := 0; ; try++ {
		c.mu.Lock()
		s := c.session
		c.mu.Unlock()
		if s == "" {
			var err error
			if s, err = c.logon(ctx); err != nil {
				return false, err
			}
			c.mu.Lock()
			c.session = s
			c.mu.Unlock()
		}
		ok, err := c.getWith(ctx, s, path, v)
		var se *StatusError
		if try == 0 && errors.As(err, &se) && se.Code == http.StatusUnauthorized {
			c.mu.Lock()
			if c.session == s {
				c.session = ""
			}
			c.mu.Unlock()
			continue
		}
		return ok, err
	}
}

func (c *Client) getWith(ctx context.Context, session, path string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Session", session)
	req.Header.Set("Accept", "application/atom+xml")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return false, nil
	default:
		return false, &StatusError{Code: resp.StatusCode, Msg: apiError(resp)}
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(v); err != nil {
		return false, fmt.Errorf("hmc %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return true, nil
}

// apiError is the message in the HMC's error answer (an HttpErrorResponse
// with a Message), or the status.
func apiError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var e struct {
		Message string `xml:"content>HttpErrorResponse>Message"`
		Plain   string `xml:"Message"`
	}
	if xml.Unmarshal(b, &e) == nil {
		if m := strings.TrimSpace(e.Message + e.Plain); m != "" {
			return m
		}
	}
	return strings.TrimSpace(resp.Status)
}
