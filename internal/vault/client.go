// Package vault reads a HashiCorp Vault or OpenBao cluster through its HTTP
// API (docs/vault.md), with a token whose policy can only read the few
// paths listed there: health, the leader, Raft, the token's own lease and
// the PKI mounts' public certificates. The one write is the token renewing
// itself, when it's renewable. Not one secret is read.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
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

// Config is one Vault cluster in obs.yaml's vault: list.
type Config struct {
	// Name is what it's kept under: "obs-beacond:<name>". Default: the
	// URL's host name.
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`        // https://vault.example.org:8200 (a load balancer, or any server)
	TokenFile string `yaml:"token_file"` // the monitoring token, alone in the file; read again when it changes
	CAFile    string `yaml:"ca_file"`    // Vault's CA, PEM; else the system's
	// Fingerprint pins Vault's certificate (SHA-256, hex, colons or not)
	// when it isn't signed by a CA obs-beacond trusts.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 60s
	// MinLifetime is how long a leaf certificate must be meant to last
	// to be judged; shorter ones are counted (docs/vault.md): 7 days.
	MinLifetime time.Duration `yaml:"min_lifetime"`
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Every == 0 {
		c.Every = time.Minute
	}
	if c.MinLifetime == 0 {
		c.MinLifetime = 7 * 24 * time.Hour
	}
	if c.Name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			c.Name = strings.ToLower(u.Hostname())
		}
	}
}

// Check says what's wrong with c, after Defaults.
func (c *Config) Check() error {
	u, err := url.Parse(strings.TrimRight(c.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("vault: url %q: want https://vault:8200", c.URL)
	}
	if c.TokenFile == "" {
		return fmt.Errorf("vault %s: token_file is needed", c.Name)
	}
	if c.Every < 10*time.Second {
		return fmt.Errorf("vault %s: every is at least 10s", c.Name)
	}
	return nil
}

// Client asks one Vault cluster. One goroutine reads with it at a time.
type Client struct {
	cfg  Config
	base string
	hc   *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time // the file's modification time when read
	renewed time.Time // when the token last renewed itself

	certs certCache // certificates fetched, kept: their ends never change

	// Audit failures: the last total read, and when it last went up.
	auditSeen   bool
	auditTotal  float64
	auditNew    float64
	auditFailed time.Time
}

// New checks cfg and the CA; the token is read when first needed.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	return newClient(cfg)
}

// newClient is New without the https check: the tests' dev-mode Vault
// answers over http.
func newClient(cfg Config) (*Client, error) {
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("vault %s: %w", cfg.Name, err)
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	return &Client{cfg: cfg, base: strings.TrimRight(cfg.URL, "/"), hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

// Name is what the cluster is kept under.
func (c *Client) Name() string { return c.cfg.Name }

// Config is the client's settings, defaults filled in.
func (c *Client) Config() Config { return c.cfg }

// readToken reads the token file when it's new or has changed, so a
// rotated token is used without a restart.
func (c *Client) readToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(c.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("the token file: %w", err)
	}
	if c.token != "" && fi.ModTime().Equal(c.tokenAt) {
		return c.token, nil
	}
	b, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("the token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("the token file is empty")
	}
	c.token, c.tokenAt = t, fi.ModTime()
	return t, nil
}

// StatusError is Vault refusing a call, with its own words.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Msg) }

// call asks /v1/<path>. auth sends the token. ok are status codes that
// carry a body worth reading besides 200 (sys/health's). v may be nil.
func (c *Client) call(ctx context.Context, method, path string, auth bool, body any, v any, ok ...int) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/v1/"+path, rd)
	if err != nil {
		return 0, err
	}
	if auth {
		t, err := c.readToken()
		if err != nil {
			return 0, err
		}
		req.Header.Set("X-Vault-Token", t)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	good := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent
	for _, o := range ok {
		good = good || resp.StatusCode == o
	}
	if !good {
		return resp.StatusCode, &StatusError{Code: resp.StatusCode, Msg: apiError(resp)}
	}
	if v == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: %w", path, err)
	}
	return resp.StatusCode, nil
}

// get reads a path, with the token.
func (c *Client) get(ctx context.Context, path string, v any) error {
	_, err := c.call(ctx, http.MethodGet, path, true, nil, v)
	return err
}

// list lists a path's keys: Vault's LIST. Nothing there is an empty
// list, not an error (Vault answers 404).
func (c *Client) list(ctx context.Context, path string) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	_, err := c.call(ctx, "LIST", path, true, nil, &r)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, nil
	}
	return r.Data.Keys, err
}

// apiError is the message in Vault's error answer, or the status.
func apiError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
		var m []string
		for _, x := range e.Errors {
			if x = strings.TrimSpace(x); x != "" {
				m = append(m, strings.ReplaceAll(x, "\n", " "))
			}
		}
		if len(m) > 0 {
			return strings.Join(m, "; ")
		}
	}
	return strings.TrimSpace(resp.Status)
}
