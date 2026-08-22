package delivery

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/alerting/internal/notifystore"
)

func TestBuildPayloadGenericDefaultShape(t *testing.T) {
	target := notifystore.Target{Kind: notifystore.KindWebhook}
	value := 142.0
	payload, err := BuildPayload(target, Event{
		RuleID: "r1", RuleName: "High error rate", EventType: "firing",
		Value: &value, Timestamp: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got defaultGenericPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if got.RuleName != "High error rate" || got.EventType != "firing" || got.Value == nil || *got.Value != 142.0 {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestBuildPayloadGenericUsesTemplate(t *testing.T) {
	tmpl := `{"custom": "{{.RuleName}} is {{.EventType}}"}`
	target := notifystore.Target{Kind: notifystore.KindWebhook, PayloadTemplate: &tmpl}
	payload, err := BuildPayload(target, Event{RuleName: "disk full", EventType: "firing"})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshaling templated payload: %v", err)
	}
	if got["custom"] != "disk full is firing" {
		t.Fatalf("got %q", got["custom"])
	}
}

func TestBuildPayloadSlackShape(t *testing.T) {
	target := notifystore.Target{Kind: notifystore.KindSlack}
	payload, err := BuildPayload(target, Event{RuleName: "High error rate", EventType: "resolved"})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got slackPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshaling slack payload: %v", err)
	}
	if got.Text == "" {
		t.Fatalf("expected non-empty Slack text")
	}
}

func TestBuildPayloadPagerDutyMapsEventActionAndRoutingKey(t *testing.T) {
	secret := "R0UT1NGKEY"
	target := notifystore.Target{Kind: notifystore.KindPagerDuty, Secret: &secret}

	firing, err := BuildPayload(target, Event{RuleID: "r1", RuleName: "disk full", EventType: "firing"})
	if err != nil {
		t.Fatalf("BuildPayload firing: %v", err)
	}
	var gotFiring pagerDutyPayload
	if err := json.Unmarshal(firing, &gotFiring); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if gotFiring.EventAction != "trigger" || gotFiring.RoutingKey != secret || gotFiring.DedupKey != "r1" {
		t.Fatalf("unexpected firing payload: %+v", gotFiring)
	}

	resolved, err := BuildPayload(target, Event{RuleID: "r1", RuleName: "disk full", EventType: "resolved"})
	if err != nil {
		t.Fatalf("BuildPayload resolved: %v", err)
	}
	var gotResolved pagerDutyPayload
	if err := json.Unmarshal(resolved, &gotResolved); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if gotResolved.EventAction != "resolve" {
		t.Fatalf("expected event_action=resolve, got %q", gotResolved.EventAction)
	}
}
