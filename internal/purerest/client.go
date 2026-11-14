// Package purerest asks a Pure Storage array's REST API 2, read only, as
// FlashArray and FlashBlade both offer it (docs/pure.md): the newest 2.x
// the array has, an API token traded for a session, lists paged with
// continuation tokens. Only GET is asked, after logging in.
package purerest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/tlspin"
)

// Config is one array in the agent's pure: section.
type Config struct {
	// Type is what it is: flasharray (the default) or flashblade.
	Type string `yaml:"type"`
	// Name is what the array is kept under, beside the agent's name
	// ("mgmt1:fa-prod-01"): default its host name.
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`        // https://fa-prod-01 (its management address)
	TokenFile string `yaml:"token_file"` // a readonly user's API token, alone in the file
	CAFile    string `yaml:"ca_file"`    // the array's CA, PEM; else the system's
	// Fingerprint pins the array's certificate (SHA-256, hex, colons or
	// not) when it isn't signed by a CA the agent trusts: the self-signed
	// one it ships with.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 60s
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Type == "" {
		c.Type = FlashArray
	}
	if c.Every == 0 {
		c.Every = time.Minute
	}
	if c.Name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			c.Name = strings.ToLower(u.Hostname())
		}
	}
}

// The two kinds of array.
const (
	FlashArray = "flasharray"
	FlashBlade = "flashblade"
)

// Client asks one array, read only. It logs in when it has no session and
// again when the array forgets it. One goroutine uses it.
type Client struct {
	kind    string // FlashArray or FlashBlade: its errors say which
	base    string // https://host
	name    string
	token   string
	hc      *http.Client
	version string // the REST version used: the newest 2.x the array offers
	session string // x-auth-token
}

// New checks cfg and reads the token and the CA; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	k := cfg.Type
	if k != FlashArray && k != FlashBlade {
		return nil, fmt.Errorf("pure: type %q: want flasharray or flashblade", cfg.Type)
	}
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%s: url %q: want https://array", k, cfg.URL)
	}
	b, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("%s: the API token: %w", k, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return nil, fmt.Errorf("%s: the token file is empty", k)
	}
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	tr := &http.Transport{TLSClientConfig: tc, MaxIdleConns: 4, IdleConnTimeout: time.Minute}
	return &Client{kind: k, base: u.Scheme + "://" + u.Host, name: cfg.Name, token: tok,
		hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

// Name is what the array is kept under.
func (c *Client) Name() string { return c.name }

// Version is the REST version in use, once logged in.
func (c *Client) Version() string { return c.version }

// StatusError is the array refusing a call.
type StatusError struct {
	Kind string
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %d %s", e.Kind, e.Code, e.Msg) }

// pickVersion is the newest 2.x the array offers, from /api/api_version.
func (c *Client) pickVersion(ctx context.Context) (string, error) {
	var v struct {
		Version []string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/api_version", nil, &v); err != nil {
		return "", err
	}
	best, bestMinor := "", -1
	for _, s := range v.Version {
		major, minor, ok := strings.Cut(s, ".")
		if !ok || major != "2" {
			continue
		}
		if m, err := strconv.Atoi(minor); err == nil && m > bestMinor {
			best, bestMinor = s, m
		}
	}
	if best == "" {
		need := "Purity 6.0 or later"
		if c.kind == FlashBlade {
			need = "Purity//FB 3.0 or later"
		}
		return "", fmt.Errorf("%s: the array offers no REST 2 (it offers %s): %s is needed", c.kind, strings.Join(v.Version, ", "), need)
	}
	return best, nil
}

// loginPath is where the token is traded: a FlashArray's is under the
// version, a FlashBlade's isn't.
func (c *Client) loginPath(what string) string {
	if c.kind == FlashBlade {
		return "/api/" + what
	}
	return "/api/" + c.version + "/" + what
}

// login trades the API token for a session.
func (c *Client) login(ctx context.Context) error {
	if c.version == "" {
		v, err := c.pickVersion(ctx)
		if err != nil {
			return err
		}
		c.version = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.loginPath("login"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("api-token", c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := "logging in: " + apiError(resp)
		if resp.StatusCode == http.StatusNotFound {
			other := map[string]string{FlashArray: FlashBlade, FlashBlade: FlashArray}[c.kind]
			msg += " (is it a " + other + "? then say type: " + other + ")"
		}
		return &StatusError{Kind: c.kind, Code: resp.StatusCode, Msg: msg}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	s := resp.Header.Get("x-auth-token")
	if s == "" {
		return fmt.Errorf("%s: logged in, but no x-auth-token came back", c.kind)
	}
	c.session = s
	return nil
}

// pageSize is how many items a list asks for at once.
const pageSize = 500

// maxPages stops a list that never ends: a million of anything.
const maxPages = 2000

// List reads every page of a REST 2 list into out. query is extra
// parameters (a filter, destroyed=false).
func List[T any](ctx context.Context, c *Client, path string, query url.Values, out *[]T) error {
	token := ""
	for range maxPages {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		for k, v := range query {
			q[k] = v
		}
		if token != "" {
			q.Set("continuation_token", token)
		}
		var env struct {
			Items             []T    `json:"items"`
			ContinuationToken string `json:"continuation_token"`
		}
		if err := c.get(ctx, path+"?"+q.Encode(), &env); err != nil {
			return err
		}
		*out = append(*out, env.Items...)
		if env.ContinuationToken == "" || len(env.Items) == 0 {
			return nil
		}
		token = env.ContinuationToken
	}
	return fmt.Errorf("%s %s: over %d pages", c.kind, path, maxPages)
}

// get reads /api/<version><path> into v, logging in first when there's no
// session and once more when the array has forgotten it.
func (c *Client) get(ctx context.Context, path string, v any) error {
	if c.session == "" {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	err := c.do(ctx, http.MethodGet, "/api/"+c.version+path, map[string]string{"x-auth-token": c.session}, v)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		c.session = ""
		if err := c.login(ctx); err != nil {
			return err
		}
		err = c.do(ctx, http.MethodGet, "/api/"+c.version+path, map[string]string{"x-auth-token": c.session}, v)
	}
	return err
}

func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	for k, h := range headers {
		req.Header.Set(k, h)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Kind: c.kind, Code: resp.StatusCode, Msg: apiError(resp)}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(v); err != nil {
		return fmt.Errorf("%s %s: %w", c.kind, strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// Logout ends the session, when the agent stops.
func (c *Client) Logout(ctx context.Context) {
	if c.session == "" || c.version == "" {
		return
	}
	_ = c.do(ctx, http.MethodPost, c.loginPath("logout"), map[string]string{"x-auth-token": c.session}, &struct{}{})
	c.session = ""
}

// apiError is the message in a REST 2 error, or the status.
func apiError(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Errors []struct {
			Message string `json:"message"`
			Context string `json:"context"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
		var m []string
		for _, x := range e.Errors {
			if x.Message != "" {
				m = append(m, x.Message)
			}
		}
		sort.Strings(m)
		if len(m) > 0 {
			return strings.Join(m, "; ")
		}
	}
	return strings.TrimSpace(resp.Status)
}
