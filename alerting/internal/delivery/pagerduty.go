package delivery

import (
	"encoding/json"
	"fmt"

	"github.com/cairnobs/cairnobs/alerting/internal/notifystore"
)

// pagerDutyPayload is PagerDuty's Events API v2 shape. routing_key comes
// from the target's `secret` field -- PagerDuty calls this an
// "integration key," not a delivery credential in the auth sense, but
// it's stored in the same plaintext `secret` column as any other
// target's credential (see /docs/phase-3-alerting-design.md's "Known
// gaps").
type pagerDutyPayload struct {
	RoutingKey  string                `json:"routing_key"`
	EventAction string                `json:"event_action"` // "trigger" | "resolve"
	DedupKey    string                `json:"dedup_key"`    // rule_id -- lets PagerDuty correlate trigger/resolve as one incident
	Payload     pagerDutyEventPayload `json:"payload"`
}

type pagerDutyEventPayload struct {
	Summary  string `json:"summary"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
}

func buildPagerDutyPayload(target notifystore.Target, event Event) ([]byte, error) {
	action := "trigger"
	severity := "critical"
	if event.EventType == "resolved" {
		action = "resolve"
		severity = "info"
	}

	var routingKey string
	if target.Secret != nil {
		routingKey = *target.Secret
	}

	summary := fmt.Sprintf("%s: %s", event.RuleName, event.EventType)
	if event.Value != nil && event.ThresholdValue != nil {
		summary = fmt.Sprintf("%s (value %.4g %s %.4g)", summary, *event.Value, comparatorSymbol(event.Comparator), *event.ThresholdValue)
	}

	return json.Marshal(pagerDutyPayload{
		RoutingKey:  routingKey,
		EventAction: action,
		DedupKey:    event.RuleID,
		Payload: pagerDutyEventPayload{
			Summary:  summary,
			Source:   "cairnobs",
			Severity: severity,
		},
	})
}
