package delivery

import (
	"encoding/json"
	"fmt"
)

// slackPayload is Slack's incoming-webhook shape: {"text": "..."}. No
// user template for this kind -- see webhook.go's BuildPayload doc
// comment for why (thin formatter, not a user-configurable path).
type slackPayload struct {
	Text string `json:"text"`
}

func buildSlackPayload(event Event) ([]byte, error) {
	var text string
	switch event.EventType {
	case "firing":
		text = fmt.Sprintf(":rotating_light: *%s* is firing", event.RuleName)
		if event.Value != nil && event.ThresholdValue != nil {
			text += fmt.Sprintf(" (value %.4g %s %.4g)", *event.Value, comparatorSymbol(event.Comparator), *event.ThresholdValue)
		}
	case "resolved":
		text = fmt.Sprintf(":white_check_mark: *%s* resolved", event.RuleName)
	default:
		text = fmt.Sprintf("%s: %s", event.RuleName, event.EventType)
	}
	return json.Marshal(slackPayload{Text: text})
}

func comparatorSymbol(c string) string {
	switch c {
	case "gt":
		return ">"
	case "gte":
		return ">="
	case "lt":
		return "<"
	case "lte":
		return "<="
	case "eq":
		return "=="
	case "ne":
		return "!="
	default:
		return c
	}
}
