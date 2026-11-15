package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The demo's alert channels (fleet run -channels DIR): one catcher on
// loopback stands in for ntfy, a Discord webhook, PagerDuty's Events API,
// a plain webhook and an email relay (SMTP on the next port), and keeps
// what each was sent. Its page,
// http://127.0.0.1:58450/, lists them newest first, as each channel's
// people would have read them; /messages.json has the same.
const channelsPort = 58450

type caught struct {
	At      time.Time         `json:"at"`
	Channel string            `json:"channel"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

type catcher struct {
	mu   sync.Mutex
	msgs []caught // newest last, at most 500
}

func serveChannels(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", channelsPort))
	if err != nil {
		log.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	files := map[string]string{
		"ntfy-topic":     "fleet-demo-alerts",
		"discord-hook":   base + "/discord/api/webhooks/1/fleet-demo",
		"webhook-url":    base + "/webhook",
		"webhook-secret": "fleet-demo-signing-secret",
		"pagerduty-key":  "fleet0demo0routing0key0000000000",
		"email-password": "fleet-demo-relay-password",
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	c := &catcher{}
	srv := &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	sl, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", channelsPort+1))
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); _ = sl.Close() }()
	go c.serveSMTP(sl, files["email-password"])
	f := func(name string) string { return filepath.Join(dir, name) }
	yaml := fmt.Sprintf(`alerts:
  public_url: http://localhost:5420
  channels:
    - {id: ops-ntfy, type: ntfy, server: %s/ntfy, topic_file: %s}
    - {id: ops-discord, type: discord, webhook_file: %s, mention_critical: true}
    - {id: on-call, type: pagerduty, min: critical, server: %s/pagerduty, routing_key_file: %s}
    - {id: tickets, type: webhook, url_file: %s, secret_file: %s}
    - {id: ops-email, type: email, smtp: %s, stand_in: true, from: "Cairn OBS <obs@example.com>", to: [ops@example.com], password_file: %s}
reports:
  digest: {weekday: monday, at: "08:00", channels: [ops-email, ops-ntfy, ops-discord]}
`, base, f("ntfy-topic"), f("discord-hook"), base, f("pagerduty-key"), f("webhook-url"), f("webhook-secret"), sl.Addr(), f("email-password"))
	p := filepath.Join(dir, "channels.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet: alert channels caught at %s/ (ntfy, Discord, PagerDuty, webhook, email); %s has the alerts: and reports: sections for obs-beacond.yaml", base, p)
}

func (c *catcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		c.mu.Lock()
		msgs := append([]caught(nil), c.msgs...)
		c.mu.Unlock()
		if r.URL.Path == "/messages.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(msgs)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString(`<!doctype html><meta charset="utf-8"><title>Alert channels (fleet demo)</title>
<style>body{font:14px system-ui;margin:2em;max-width:60em}article{border-top:1px solid #ccc;padding:.6em 0}
b{display:inline-block;min-width:7em}small{color:#666}pre{white-space:pre-wrap;margin:.3em 0}</style>
<h1>Alert channels</h1><p>What the fleet demo's channels were sent, newest first.</p>`)
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			fmt.Fprintf(&b, "<article><b>%s</b> <small>%s</small>", html.EscapeString(m.Channel), m.At.Local().Format("Jan 2 15:04:05"))
			for k, v := range m.Headers {
				fmt.Fprintf(&b, " <small>%s: %s</small>", html.EscapeString(k), html.EscapeString(v))
			}
			fmt.Fprintf(&b, "<pre>%s</pre></article>", html.EscapeString(m.Body))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, b.String())
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	m := caught{At: time.Now(), Headers: map[string]string{}, Body: string(body)}
	keep := func(names ...string) {
		for _, h := range names {
			if v := r.Header.Get(h); v != "" {
				m.Headers[h] = v
			}
		}
	}
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/ntfy/"):
		m.Channel = "ntfy"
		keep("Title", "Priority", "Tags", "Click")
	case strings.HasPrefix(p, "/discord/"):
		m.Channel = "Discord"
	case strings.HasPrefix(p, "/pagerduty/"):
		m.Channel = "PagerDuty"
	case p == "/webhook":
		m.Channel = "webhook"
		keep("X-Beacon-Signature")
	default:
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var v any
		if json.Unmarshal(body, &v) == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				m.Body = string(pretty)
			}
		}
	}
	c.keep(m)
	switch m.Channel {
	case "PagerDuty":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"success","message":"Event processed"}`)
	case "Discord":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fleet","event":"message"}`)
	}
}

// serveSMTP takes mail as a relay would: signed in with the password (AUTH
// PLAIN; the channel only signs in in the clear because it's loopback),
// then one message per DATA, kept like the rest.
func (c *catcher) serveSMTP(l net.Listener, password string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Minute))
			r := bufio.NewReader(conn)
			say := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
			say("220 fleet relay ESMTP")
			authed := false
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				fs := strings.Fields(line)
				if len(fs) == 0 {
					say("500 empty")
					continue
				}
				switch strings.ToUpper(fs[0]) {
				case "EHLO", "HELO":
					say("250-fleet relay")
					say("250 AUTH PLAIN")
				case "AUTH":
					b, _ := base64.StdEncoding.DecodeString(fs[len(fs)-1])
					if parts := strings.Split(string(b), "\x00"); len(parts) == 3 && parts[2] == password {
						authed = true
						say("235 2.7.0 ok")
					} else {
						say("535 5.7.8 Authentication credentials invalid")
					}
				case "MAIL", "RCPT":
					if !authed {
						say("530 5.7.0 sign in first")
						continue
					}
					say("250 ok")
				case "DATA":
					say("354 go ahead")
					var msg strings.Builder
					for {
						l, err := r.ReadString('\n')
						if err != nil {
							return
						}
						if l == ".\r\n" {
							break
						}
						if msg.Len() < 64<<10 {
							msg.WriteString(strings.TrimPrefix(l, "."))
						}
					}
					m := caught{At: time.Now(), Channel: "email", Headers: map[string]string{}}
					head, body, _ := strings.Cut(msg.String(), "\r\n\r\n")
					for _, h := range strings.Split(head, "\r\n") {
						if k, v, ok := strings.Cut(h, ": "); ok && (k == "Subject" || k == "To") {
							if d, err := new(mime.WordDecoder).DecodeHeader(v); err == nil {
								v = d
							}
							m.Headers[k] = v
						}
					}
					m.Body = strings.ReplaceAll(body, "\r\n", "\n")
					c.keep(m)
					say("250 2.0.0 queued")
				case "QUIT":
					say("221 bye")
					return
				default:
					say("250 ok")
				}
			}
		}()
	}
}

func (c *catcher) keep(m caught) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	if len(c.msgs) > 500 {
		c.msgs = c.msgs[len(c.msgs)-500:]
	}
	c.mu.Unlock()
}
