// Package notify delivers alert notices: ntfy, Discord, a signed webhook,
// email, PagerDuty, or the log. Secrets (topics, tokens, webhook URLs, passwords) are
// read from files, or handed over directly for channels set in the UI, and
// never appear in errors or logs; errors name the channel by its id.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/version"
)

// Notice is one thing to tell people.
type Notice struct {
	Kind     string    `json:"kind"` // firing, escalated, still, acknowledged (to Followers only), resolved, test, digest
	ID       string    `json:"id"`
	Severity string    `json:"severity"` // warning, serious, critical
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	Host     string    `json:"host,omitempty"`
	URL      string    `json:"url,omitempty"` // link back to Cairn OBS
	Since    time.Time `json:"since"`
}

// Rank orders severities; a channel takes notices at or above its minimum.
func Rank(sev string) int {
	switch sev {
	case "info":
		return 1
	case "warning":
		return 2
	case "serious":
		return 3
	case "critical":
		return 4
	}
	return 0
}

// Config is one entry of alerts.channels in obs-beacond.yaml.
type Config struct {
	ID   string `yaml:"id"`
	Type string `yaml:"type"` // ntfy, discord, webhook, email, pagerduty, log
	Min  string `yaml:"min"`  // lowest severity sent (default warning)

	Server          string `yaml:"server"`           // ntfy: https://ntfy.sh or our own; pagerduty: the Events API (EU accounts: https://events.eu.pagerduty.com)
	TopicFile       string `yaml:"topic_file"`       // ntfy: the topic is a secret of sorts
	TokenFile       string `yaml:"token_file"`       // ntfy: optional access token
	WebhookFile     string `yaml:"webhook_file"`     // discord: the webhook URL
	MentionCritical bool   `yaml:"mention_critical"` // discord: @here when something goes critical
	URLFile         string `yaml:"url_file"`         // webhook: where to POST
	SecretFile      string `yaml:"secret_file"`      // webhook: HMAC-SHA256 signing secret, optional
	RoutingKeyFile  string `yaml:"routing_key_file"` // pagerduty: the Events API v2 integration key

	SMTP         string   `yaml:"smtp"`          // email: relay host:port (587 STARTTLS, or 465 TLS)
	Username     string   `yaml:"username"`      // email: defaults to from's address
	PasswordFile string   `yaml:"password_file"` // email
	From         string   `yaml:"from"`          // email: e.g. "Cairn OBS <obs@example.org>"
	To           []string `yaml:"to"`            // email

	// Secrets given directly rather than in files: channels set in the
	// UI keep theirs sealed in the database. A value here wins over its file.
	Secrets Secrets `yaml:"-"`
}

// Secrets are a channel's secret values, for channels whose secrets aren't
// in files.
type Secrets struct {
	Topic      string `yaml:"topic,omitempty"`       // ntfy
	Token      string `yaml:"token,omitempty"`       // ntfy, optional
	Webhook    string `yaml:"webhook,omitempty"`     // discord
	URL        string `yaml:"url,omitempty"`         // webhook
	Secret     string `yaml:"secret,omitempty"`      // webhook, optional
	Password   string `yaml:"password,omitempty"`    // email
	RoutingKey string `yaml:"routing_key,omitempty"` // pagerduty
}

// Channel sends notices somewhere.
type Channel interface {
	ID() string
	Type() string
	Min() string
	Send(ctx context.Context, n Notice) error
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return v, nil
}

// secret is a secret given directly, or else read from its file. Errors name
// the setting, never the value.
func (c Config) secret(inline, file, key string) (string, error) {
	if v := strings.TrimSpace(inline); v != "" {
		return v, nil
	}
	if file == "" {
		return "", fmt.Errorf("alert channel %s: %s is missing", c.ID, key)
	}
	v, err := readFile(file)
	if err != nil {
		return "", fmt.Errorf("alert channel %s: %s_file: %w", c.ID, key, err)
	}
	return v, nil
}

// New builds a channel, reading its secrets now so a bad config fails at start.
func New(c Config) (Channel, error) {
	if c.ID == "" {
		return nil, fmt.Errorf("alert channel needs an id")
	}
	if c.Min == "" {
		c.Min = "warning"
	}
	if Rank(c.Min) == 0 {
		return nil, fmt.Errorf("alert channel %s: min %q isn't warning, serious or critical", c.ID, c.Min)
	}
	base := base{id: c.ID, typ: c.Type, min: c.Min, hc: &http.Client{Timeout: 10 * time.Second}}
	switch c.Type {
	case "ntfy":
		if c.Server == "" {
			c.Server = "https://ntfy.sh"
		}
		u, err := url.Parse(c.Server)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("alert channel %s: server %q isn't a URL", c.ID, c.Server)
		}
		topic, err := c.secret(c.Secrets.Topic, c.TopicFile, "topic")
		if err != nil {
			return nil, err
		}
		n := &ntfy{base: base, endpoint: strings.TrimRight(c.Server, "/") + "/" + url.PathEscape(topic)}
		if c.TokenFile != "" || c.Secrets.Token != "" {
			if n.token, err = c.secret(c.Secrets.Token, c.TokenFile, "token"); err != nil {
				return nil, err
			}
		}
		return n, nil
	case "discord":
		hook, err := c.secret(c.Secrets.Webhook, c.WebhookFile, "webhook")
		if err != nil {
			return nil, err
		}
		return &discord{base: base, hook: hook, mention: c.MentionCritical}, nil
	case "webhook":
		to, err := c.secret(c.Secrets.URL, c.URLFile, "url")
		if err != nil {
			return nil, err
		}
		w := &webhook{base: base, to: to}
		if c.SecretFile != "" || c.Secrets.Secret != "" {
			if w.secret, err = c.secret(c.Secrets.Secret, c.SecretFile, "secret"); err != nil {
				return nil, err
			}
		}
		return w, nil
	case "pagerduty":
		if c.Server == "" {
			c.Server = DefaultPagerDutyServer
		}
		u, err := url.Parse(c.Server)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
			return nil, fmt.Errorf("alert channel %s: server %q isn't a URL", c.ID, c.Server)
		}
		key, err := c.secret(c.Secrets.RoutingKey, c.RoutingKeyFile, "routing_key")
		if err != nil {
			return nil, err
		}
		return &pagerduty{base: base, server: strings.TrimRight(c.Server, "/"), key: key}, nil
	case "log":
		return &logCh{base: base}, nil
	case "email":
		return newEmail(base, c)
	}
	return nil, fmt.Errorf("alert channel %s: unknown type %q", c.ID, c.Type)
}

type base struct {
	id, typ, min string
	hc           *http.Client
}

func (b *base) ID() string   { return b.id }
func (b *base) Type() string { return b.typ }
func (b *base) Min() string  { return b.min }

// post sends a request and turns a non-2xx answer into an error that names
// the channel, never the URL (which may be the secret).
func (b *base) post(ctx context.Context, url, contentType string, body []byte, hdr map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: bad request", b.id)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "obs-beacond/"+version.Version)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: can't reach it", b.id)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: answered %s", b.id, resp.Status)
	}
	return nil
}

// headline is the one line every channel leads with.
func headline(n Notice) string {
	switch n.Kind {
	case "resolved":
		return "Resolved: " + n.Title
	case "still":
		return "Still " + n.Severity + ": " + n.Title
	case "escalated":
		return "Now " + n.Severity + ": " + n.Title
	case "test":
		return "Test from Cairn OBS"
	case "digest":
		return n.Title
	}
	return strings.ToUpper(n.Severity[:1]) + n.Severity[1:] + ": " + n.Title
}

func body(n Notice) string {
	if n.Kind == "digest" {
		return n.Detail // the whole text, written for this
	}
	var b strings.Builder
	if n.Detail != "" {
		b.WriteString(n.Detail + "\n")
	}
	if n.Kind == "test" {
		b.WriteString("If you can read this, alerts reach this channel.\n")
	}
	if n.Host != "" {
		b.WriteString("Host: " + n.Host + "\n")
	}
	if !n.Since.IsZero() && n.Kind != "test" {
		b.WriteString("Since: " + n.Since.UTC().Format("2006-01-02 15:04 UTC") + "\n")
	}
	if n.ID != "" {
		b.WriteString("Incident " + n.ID)
	}
	return strings.TrimSpace(b.String())
}

type ntfy struct {
	base
	endpoint string
	token    string
}

func (c *ntfy) Send(ctx context.Context, n Notice) error {
	priority, tag := "default", "warning"
	switch {
	case n.Kind == "resolved":
		priority, tag = "low", "white_check_mark"
	case n.Kind == "test":
		priority, tag = "default", "test_tube"
	case n.Severity == "critical":
		priority, tag = "urgent", "rotating_light"
	case n.Severity == "serious":
		priority, tag = "high", "large_orange_diamond"
	}
	hdr := map[string]string{"Title": headline(n), "Priority": priority, "Tags": tag}
	if n.URL != "" {
		hdr["Click"] = n.URL
	}
	if c.token != "" {
		hdr["Authorization"] = "Bearer " + c.token
	}
	return c.post(ctx, c.endpoint, "text/plain; charset=utf-8", []byte(body(n)), hdr)
}

type discord struct {
	base
	hook    string
	mention bool
}

var discordColor = map[string]int{"warning": 0xf2d43d, "serious": 0xd65cff, "critical": 0xff4545}

func (c *discord) Send(ctx context.Context, n Notice) error {
	color := discordColor[n.Severity]
	switch n.Kind {
	case "resolved":
		color = 0x3dd68c
	case "test", "digest":
		color = 0x3fb6ff
	}
	embed := map[string]any{
		"title": headline(n), "description": body(n), "color": color,
		"timestamp": time.Now().UTC().Format(time.RFC3339), "footer": map[string]string{"text": "Cairn OBS"},
	}
	if n.URL != "" {
		embed["url"] = n.URL
	}
	msg := map[string]any{"username": "Cairn OBS", "embeds": []any{embed}, "allowed_mentions": map[string]any{"parse": []string{}}}
	if c.mention && n.Severity == "critical" && (n.Kind == "firing" || n.Kind == "escalated") {
		msg["content"] = "@here"
		msg["allowed_mentions"] = map[string]any{"parse": []string{"everyone"}}
	}
	b, _ := json.Marshal(msg)
	return c.post(ctx, c.hook, "application/json", b, nil)
}

type webhook struct {
	base
	to     string
	secret string
}

// Send POSTs the notice as JSON. With a secret, X-Beacon-Signature carries
// sha256=<hex HMAC of the body>, so the receiver can tell it came from us.
func (c *webhook) Send(ctx context.Context, n Notice) error {
	b, _ := json.Marshal(n)
	hdr := map[string]string{}
	if c.secret != "" {
		m := hmac.New(sha256.New, []byte(c.secret))
		m.Write(b)
		hdr["X-Beacon-Signature"] = "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	return c.post(ctx, c.to, "application/json", b, hdr)
}

type logCh struct{ base }

func (c *logCh) Send(_ context.Context, n Notice) error {
	log.Printf("alert [%s] %s", c.id, strings.ReplaceAll(headline(n)+" | "+body(n), "\n", " | "))
	return nil
}
