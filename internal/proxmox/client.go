// Package proxmox reads a Proxmox VE cluster's VMs, containers and nodes
// from its API (docs/cluster-watcher.md, step 7), with an API token that
// holds the built-in PVEAuditor role: it can look, never change. The agent
// on one node reads the whole cluster through that node's local API and
// sends it as a cluster agent would.
package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/tlspin"
)

// Config is the agent's proxmox: section.
type Config struct {
	// Name is what the cluster is kept under, beside the agent's name
	// ("pve1:proxmox"): default proxmox.
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`        // default https://127.0.0.1:8006
	TokenFile string `yaml:"token_file"` // holds USER@REALM!TOKENID=SECRET
	CAFile    string `yaml:"ca_file"`    // default /etc/pve/pve-root-ca.pem
	// Fingerprint pins the API's certificate instead (SHA-256, hex, colons
	// or not), for when the agent can't read the cluster's CA.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 30s
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Name == "" {
		c.Name = "proxmox"
	}
	if c.URL == "" {
		c.URL = "https://127.0.0.1:8006"
	}
	if c.CAFile == "" && c.Fingerprint == "" {
		c.CAFile = "/etc/pve/pve-root-ca.pem"
	}
	if c.Every == 0 {
		c.Every = 30 * time.Second
	}
}

// Client asks one Proxmox API, read only.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New makes a client from cfg, reading the token and the CA now.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	b, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("proxmox: the token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if !strings.Contains(tok, "!") || !strings.Contains(tok, "=") {
		return nil, errors.New("proxmox: the token file holds USER@REALM!TOKENID=SECRET")
	}
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("proxmox: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	return &Client{base: strings.TrimRight(cfg.URL, "/") + "/api2/json", token: tok, hc: &http.Client{Transport: tr, Timeout: 20 * time.Second}}, nil
}

// StatusError is the API refusing a call.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("proxmox: %d %s", e.Code, e.Msg) }

// get reads path's "data" into v. Only GET exists here.
func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		m := strings.TrimSpace(resp.Status)
		if s := strings.TrimSpace(string(msg)); s != "" && !strings.HasPrefix(s, "{") {
			m = s
		}
		return &StatusError{Code: resp.StatusCode, Msg: m}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&env); err != nil {
		return fmt.Errorf("proxmox %s: %w", path, err)
	}
	return json.Unmarshal(env.Data, v)
}

func esc(s string) string { return url.PathEscape(s) }
