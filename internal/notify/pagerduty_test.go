package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// Built from PagerDuty's Events API v2 documentation; no live account here.
func TestPagerDutyFollowsTheIncident(t *testing.T) {
	s, got := sink(t, 202)
	ch, err := New(Config{ID: "oncall", Type: "pagerduty", Server: s.URL + "/", RoutingKeyFile: secret(t, "R0UT1NGKEY0123456789abcdefghijkl")})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := ch.(Follower); !ok || !f.Follows() {
		t.Fatal("pagerduty doesn't follow incidents")
	}
	send := func(n Notice) map[string]any {
		t.Helper()
		if err := ch.Send(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		c := <-got
		var m map[string]any
		if err := json.Unmarshal(c.body, &m); err != nil {
			t.Fatal(err)
		}
		m["_path"] = c.path
		return m
	}

	m := send(crit)
	p, _ := m["payload"].(map[string]any)
	if m["_path"] != "/v2/enqueue" || m["event_action"] != "trigger" || m["dedup_key"] != "cairn-obs/B-0001" || m["routing_key"] != "R0UT1NGKEY0123456789abcdefghijkl" ||
		p["severity"] != "critical" || p["source"] != "node2" || p["summary"] != crit.Title || p["timestamp"] != "2026-10-15T18:00:00Z" ||
		m["client_url"] != crit.URL {
		t.Fatalf("trigger: %v", m)
	}
	serious := crit
	serious.Kind, serious.Severity = "escalated", "serious"
	if p, _ := send(serious)["payload"].(map[string]any); p["severity"] != "error" {
		t.Fatalf("serious maps to %v", p["severity"])
	}
	for kind, action := range map[string]string{"acknowledged": "acknowledge", "resolved": "resolve"} {
		n := crit
		n.Kind = kind
		if m := send(n); m["event_action"] != action || m["dedup_key"] != "cairn-obs/B-0001" || m["payload"] != nil {
			t.Fatalf("%s: %v", kind, m)
		}
	}
	// A test is a change event: it proves the key and pages nobody.
	if m := send(Notice{Kind: "test", Severity: "warning", Title: "Test"}); m["_path"] != "/v2/change/enqueue" || m["event_action"] != nil {
		t.Fatalf("test: %v", m)
	}
	// Repeats and digests aren't sent: PagerDuty escalates on its own.
	for _, kind := range []string{"still", "digest"} {
		n := crit
		n.Kind = kind
		if err := ch.Send(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case c := <-got:
		t.Fatalf("sent a repeat or digest: %s", c.body)
	default:
	}
}

func TestPagerDutyErrorsHideTheKey(t *testing.T) {
	s, _ := sink(t, 400)
	ch, err := New(Config{ID: "oncall", Type: "pagerduty", Server: s.URL, Secrets: Secrets{RoutingKey: "SECRETKEY"}})
	if err != nil {
		t.Fatal(err)
	}
	err = ch.Send(context.Background(), crit)
	if err == nil || strings.Contains(err.Error(), "SECRETKEY") || !strings.Contains(err.Error(), "oncall") {
		t.Fatalf("error: %v", err)
	}
	if _, err := New(Config{ID: "oncall", Type: "pagerduty"}); err == nil || !strings.Contains(err.Error(), "routing_key") {
		t.Fatalf("no key: %v", err)
	}
	if c := clip(strings.Repeat("é", 600), 1024); len(c) > 1024 || !utf8.ValidString(c) {
		t.Fatalf("clip: %d bytes", len(c))
	}
}
