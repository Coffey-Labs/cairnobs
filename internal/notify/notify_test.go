package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type captured struct {
	path string
	hdr  http.Header
	body []byte
}

func sink(t *testing.T, status int) (*httptest.Server, chan captured) {
	t.Helper()
	got := make(chan captured, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- captured{r.URL.Path, r.Header.Clone(), b}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s, got
}

func secret(t *testing.T, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var crit = Notice{Kind: "firing", ID: "B-0001", Severity: "critical", Title: "inbuxa-demo on host2: restarting", Detail: "Restarting (1) 3 seconds ago", Host: "host2", URL: "https://beacon.test/containers", Since: time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)}

func TestNtfy(t *testing.T) {
	s, got := sink(t, 200)
	ch, err := New(Config{ID: "phone", Type: "ntfy", Server: s.URL, TopicFile: secret(t, "beacon-abc123"), TokenFile: secret(t, "tk_secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), crit); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c.path != "/beacon-abc123" || c.hdr.Get("Priority") != "urgent" || c.hdr.Get("Tags") != "rotating_light" ||
		c.hdr.Get("Click") != crit.URL || c.hdr.Get("Authorization") != "Bearer tk_secret" ||
		c.hdr.Get("Title") != "Critical: inbuxa-demo on host2: restarting" {
		t.Fatalf("ntfy request: %s %v", c.path, c.hdr)
	}
	if !strings.Contains(string(c.body), "Host: host2") || !strings.Contains(string(c.body), "Incident B-0001") {
		t.Fatalf("ntfy body: %s", c.body)
	}
	res := crit
	res.Kind = "resolved"
	_ = ch.Send(context.Background(), res)
	if c := <-got; c.hdr.Get("Priority") != "low" || !strings.HasPrefix(c.hdr.Get("Title"), "Resolved: ") {
		t.Fatalf("resolved: %v", c.hdr)
	}
}

func TestDiscordMentionsOnlyWhenAsked(t *testing.T) {
	s, got := sink(t, 204)
	hook := secret(t, s.URL+"/api/webhooks/1/secret-token")
	for _, mention := range []bool{false, true} {
		ch, err := New(Config{ID: "ops", Type: "discord", WebhookFile: hook, MentionCritical: mention})
		if err != nil {
			t.Fatal(err)
		}
		if err := ch.Send(context.Background(), crit); err != nil {
			t.Fatal(err)
		}
		var msg struct {
			Content         string `json:"content"`
			AllowedMentions struct {
				Parse []string `json:"parse"`
			} `json:"allowed_mentions"`
			Embeds []struct {
				Title string `json:"title"`
				Color int    `json:"color"`
			} `json:"embeds"`
		}
		_ = json.Unmarshal((<-got).body, &msg)
		if mention != (msg.Content == "@here") || mention != (len(msg.AllowedMentions.Parse) == 1) {
			t.Fatalf("mention=%v: %+v", mention, msg)
		}
		if msg.Embeds[0].Color != 0xff4545 || msg.Embeds[0].Title != "Critical: inbuxa-demo on host2: restarting" {
			t.Fatalf("embed: %+v", msg.Embeds)
		}
	}
}

func TestWebhookSigns(t *testing.T) {
	s, got := sink(t, 200)
	ch, err := New(Config{ID: "hook", Type: "webhook", URLFile: secret(t, s.URL+"/in"), SecretFile: secret(t, "shh")})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), crit); err != nil {
		t.Fatal(err)
	}
	c := <-got
	m := hmac.New(sha256.New, []byte("shh"))
	m.Write(c.body)
	if c.hdr.Get("X-Beacon-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("signature %q", c.hdr.Get("X-Beacon-Signature"))
	}
}

func TestErrorsNeverShowTheSecret(t *testing.T) {
	s, _ := sink(t, 500)
	hook := s.URL + "/api/webhooks/1/very-secret-token"
	ch, _ := New(Config{ID: "ops", Type: "discord", WebhookFile: secret(t, hook)})
	err := ch.Send(context.Background(), crit)
	if err == nil || strings.Contains(err.Error(), "very-secret-token") || !strings.Contains(err.Error(), "ops") {
		t.Fatalf("error %v", err)
	}
	ch, _ = New(Config{ID: "ops", Type: "discord", WebhookFile: secret(t, "http://127.0.0.1:1/api/webhooks/1/very-secret-token")})
	if err := ch.Send(context.Background(), crit); err == nil || strings.Contains(err.Error(), "very-secret-token") {
		t.Fatalf("unreachable error %v", err)
	}
}

func TestConfigChecks(t *testing.T) {
	for _, c := range []Config{
		{ID: "", Type: "log"},
		{ID: "x", Type: "pager"},
		{ID: "x", Type: "log", Min: "loud"},
		{ID: "x", Type: "ntfy", TopicFile: "/nonexistent"},
		{ID: "x", Type: "email"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestSinceInTheGivenZone(t *testing.T) {
	az := time.FixedZone("MST", -7*3600)
	n := crit
	n.Since = n.Since.In(az)
	if b := body(n); !strings.Contains(b, "Since: 2026-10-15 11:00 MST\n") {
		t.Fatalf("body:\n%s", b)
	}
	if b := body(crit); !strings.Contains(b, "Since: 2026-10-15 18:00 UTC\n") {
		t.Fatalf("UTC body:\n%s", b)
	}
}
