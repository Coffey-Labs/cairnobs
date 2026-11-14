package notify

// PagerDuty, through its Events API v2. Unlike the other channels it follows
// each incident rather than just hearing about it: Cairn OBS's incident id is
// the dedup key, so opening one triggers a PagerDuty incident, a change of
// severity updates it, acknowledging it in Cairn OBS acknowledges it there,
// and resolving resolves it. Repeats aren't sent: PagerDuty's escalation
// policy does the nagging. A test sends a change event, which pages nobody.
//
// https://developer.pagerduty.com/docs/events-api-v2/trigger-events/

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// DefaultPagerDutyServer is the Events API for US accounts; EU accounts use
// https://events.eu.pagerduty.com.
const DefaultPagerDutyServer = "https://events.pagerduty.com"

type pagerduty struct {
	base
	server string
	key    string // the integration's routing key
}

// Follows says the channel tracks incidents, so it also wants to hear when
// one is acknowledged.
func (c *pagerduty) Follows() bool { return true }

// Follower is a channel that follows incidents: it's told about
// acknowledgements as well as the notices every channel gets.
type Follower interface {
	Follows() bool
}

var pdSeverity = map[string]string{"critical": "critical", "serious": "error", "warning": "warning", "info": "info"}

func (c *pagerduty) dedup(n Notice) string {
	if n.ID == "" {
		return ""
	}
	return "cairn-obs/" + n.ID
}

func (c *pagerduty) Send(ctx context.Context, n Notice) error {
	switch n.Kind {
	case "still", "digest":
		return nil
	case "test":
		return c.change(ctx, n)
	case "acknowledged", "resolved":
		if c.dedup(n) == "" {
			return nil
		}
		action := "acknowledge"
		if n.Kind == "resolved" {
			action = "resolve"
		}
		b, _ := json.Marshal(map[string]any{"routing_key": c.key, "event_action": action, "dedup_key": c.dedup(n)})
		return c.post(ctx, c.server+"/v2/enqueue", "application/json", b, nil)
	}
	// firing or escalated: trigger, or update the open one.
	sev := pdSeverity[n.Severity]
	if sev == "" {
		sev = "warning"
	}
	source := n.Host
	if source == "" {
		source = "Cairn OBS"
	}
	ts := n.Since
	if ts.IsZero() {
		ts = time.Now()
	}
	details := map[string]string{}
	if n.Detail != "" {
		details["detail"] = n.Detail
	}
	if n.ID != "" {
		details["incident"] = n.ID
	}
	ev := map[string]any{
		"routing_key": c.key, "event_action": "trigger",
		"payload": map[string]any{
			"summary": clip(n.Title, 1024), "source": source, "severity": sev,
			"timestamp": ts.UTC().Format(time.RFC3339), "component": n.Host,
			"custom_details": details,
		},
		"client": "Cairn OBS",
	}
	if k := c.dedup(n); k != "" {
		ev["dedup_key"] = k
	}
	if n.URL != "" {
		ev["client_url"] = n.URL
		ev["links"] = []map[string]string{{"href": n.URL, "text": "Open in Cairn OBS"}}
	}
	b, _ := json.Marshal(ev)
	return c.post(ctx, c.server+"/v2/enqueue", "application/json", b, nil)
}

// change sends a change event: it shows on the service's timeline, proves
// the key works and pages nobody.
func (c *pagerduty) change(ctx context.Context, n Notice) error {
	b, _ := json.Marshal(map[string]any{
		"routing_key": c.key,
		"payload": map[string]any{
			"summary": "Test from Cairn OBS: alerts reach this PagerDuty service", "source": "Cairn OBS",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
	})
	return c.post(ctx, c.server+"/v2/change/enqueue", "application/json", b, nil)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1] // don't cut a character in half
	}
	return s + "…"
}
