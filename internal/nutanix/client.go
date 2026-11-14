// Package nutanix reads a Prism Central's clusters, AHV hosts, VMs,
// storage containers and open alerts through its v4 API
// (docs/cluster-watcher.md, step 9), as a user or service account holding
// the built-in Prism Viewer role: it can look, never change. Only GET is
// asked.
package nutanix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/tlspin"
)

// Config is the agent's nutanix: section.
type Config struct {
	// Name is what Prism Central is kept under, beside the agent's name
	// ("mgmt1:pc-01.example.org"): default its host name.
	Name string `yaml:"name"`
	URL  string `yaml:"url"` // https://prism-central:9440
	// A service account's API key (Prism Central 2024.3 and later), or a
	// user and password: one or the other.
	APIKeyFile   string `yaml:"api_key_file"`
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"password_file"`
	CAFile       string `yaml:"ca_file"` // Prism Central's CA, PEM; else the system's
	// Fingerprint pins Prism Central's certificate (SHA-256, hex, colons or
	// not) when it isn't signed by a CA the agent trusts: its self-signed
	// one, as installed.
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

// Client asks one Prism Central, read only.
type Client struct {
	base string
	name string
	auth func(*http.Request)
	hc   *http.Client
}

// New makes a client from cfg, reading the key or password and the CA now.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("nutanix: url %q: want https://prism-central:9440", cfg.URL)
	}
	if u.Port() == "" {
		u.Host += ":9440"
	}
	c := &Client{base: u.Scheme + "://" + u.Host + "/api", name: u.Hostname()}
	switch {
	case cfg.APIKeyFile != "" && cfg.Username != "":
		return nil, errors.New("nutanix: api_key_file or username, not both")
	case cfg.APIKeyFile != "":
		b, err := os.ReadFile(cfg.APIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("nutanix: the API key: %w", err)
		}
		key := strings.TrimSpace(string(b))
		c.auth = func(r *http.Request) { r.Header.Set("X-Ntnx-Api-Key", key) }
	case cfg.Username != "":
		b, err := os.ReadFile(cfg.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("nutanix: the password: %w", err)
		}
		user, pass := cfg.Username, strings.TrimRight(string(b), "\r\n")
		c.auth = func(r *http.Request) { r.SetBasicAuth(user, pass) }
	default:
		return nil, errors.New("nutanix: no api_key_file, and no username")
	}
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("nutanix: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	c.hc = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	return c, nil
}

// Name is what Prism Central is called here: its host name.
func (c *Client) Name() string { return c.name }

// StatusError is the API refusing a call.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("nutanix: %d %s", e.Code, e.Msg) }

// pageSize is the most v4 lists give at once.
const pageSize = 100

// maxPages stops a list that never ends: 100,000 of anything.
const maxPages = 1000

// list reads every page of path into out, a pointer to a slice. filter is
// an OData $filter, or "".
func list[T any](ctx context.Context, c *Client, path, filter string, out *[]T) error {
	for page := 0; page < maxPages; page++ {
		q := url.Values{"$page": {strconv.Itoa(page)}, "$limit": {strconv.Itoa(pageSize)}}
		if filter != "" {
			q.Set("$filter", filter)
		}
		var env struct {
			Data     []T `json:"data"`
			Metadata struct {
				Total int `json:"totalAvailableResults"`
			} `json:"metadata"`
		}
		if err := c.get(ctx, path+"?"+q.Encode(), &env); err != nil {
			return err
		}
		*out = append(*out, env.Data...)
		if len(env.Data) < pageSize || (env.Metadata.Total > 0 && len(*out) >= env.Metadata.Total) {
			return nil
		}
	}
	return fmt.Errorf("nutanix %s: over %d pages", path, maxPages)
}

// get reads path's JSON into v. Only GET exists here.
func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Code: resp.StatusCode, Msg: apiError(resp)}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(v); err != nil {
		return fmt.Errorf("nutanix %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// apiError is the message in a v4 error response, or the status.
func apiError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Data struct {
			Error []struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Data.Error) > 0 && e.Data.Error[0].Message != "" {
		return e.Data.Error[0].Message
	}
	return strings.TrimSpace(resp.Status)
}
