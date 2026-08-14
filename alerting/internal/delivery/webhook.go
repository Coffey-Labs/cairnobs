// Package delivery sends notifications for firing/resolved alert
// events. All three notification_targets.kind values -- webhook, slack,
// pagerduty -- go through the exact same claim-then-POST-with-backoff
// mechanism in this file; slack.go and pagerduty.go are payload
// *formatters* only, never a separate delivery path, per
// /docs/phase-3-alerting-design.md's "thin wrappers over the generic
// webhook" requirement.
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/alerting/internal/notifystore"
)

// Event is the firing/resolved occurrence a payload is rendered from.
type Event struct {
	RuleID         string
	RuleName       string
	EventType      string // "firing" | "resolved"
	ConditionType  string
	Comparator     string
	ThresholdValue *float64
	Value          *float64
	Timestamp      time.Time
}

// defaultGenericPayload is used when a webhook target has no
// payload_template.
type defaultGenericPayload struct {
	RuleID    string   `json:"rule_id"`
	RuleName  string   `json:"rule_name"`
	EventType string   `json:"event_type"`
	Value     *float64 `json:"value,omitempty"`
	Timestamp string   `json:"timestamp"`
}

// BuildPayload dispatches by target.Kind to the right formatter. This is
// the only place kind-specific formatting logic lives -- everything
// downstream of this (worker.go's send loop) is kind-agnostic.
func BuildPayload(target notifystore.Target, event Event) ([]byte, error) {
	switch target.Kind {
	case notifystore.KindSlack:
		return buildSlackPayload(event)
	case notifystore.KindPagerDuty:
		return buildPagerDutyPayload(target, event)
	default: // webhook
		return buildGenericPayload(target, event)
	}
}

func buildGenericPayload(target notifystore.Target, event Event) ([]byte, error) {
	if target.PayloadTemplate == nil || *target.PayloadTemplate == "" {
		return json.Marshal(defaultGenericPayload{
			RuleID: event.RuleID, RuleName: event.RuleName, EventType: event.EventType,
			Value: event.Value, Timestamp: event.Timestamp.UTC().Format(time.RFC3339),
		})
	}
	tmpl, err := template.New("payload").Parse(*target.PayloadTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing payload_template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, event); err != nil {
		return nil, fmt.Errorf("rendering payload_template: %w", err)
	}
	return buf.Bytes(), nil
}

// Worker claims pending/retrying delivery_log rows and sends them.
// Deliberately separate from the evaluator: "decided to notify"
// (rulestore.ApplyTransition's transactional outbox insert) is
// transactionally certain; "the HTTP call succeeded" is best-effort with
// retries, handled entirely here.
type Worker struct {
	pool          *pgxpool.Pool
	notifications *notifystore.Store
	http          *http.Client
	logger        *slog.Logger
}

func NewWorker(pool *pgxpool.Pool, notifications *notifystore.Store, logger *slog.Logger) *Worker {
	return &Worker{pool: pool, notifications: notifications, http: &http.Client{Timeout: 10 * time.Second}, logger: logger}
}

// Run claims and sends due deliveries every tickInterval until ctx is
// cancelled.
func (w *Worker) Run(ctx context.Context, tickInterval time.Duration) error {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.processDue(ctx)
		}
	}
}

type claimedDelivery struct {
	id                   int64
	ruleID               string
	notificationTargetID string
	eventType            string
	attemptCount         int
	maxAttempts          int
	payload              []byte
}

// processDue claims due rows (same SKIP LOCKED pattern as the
// evaluator's rule claim -- see rulestore.ClaimDueRules) and attempts
// delivery for each. One claim batch per tick; a stuck/slow target
// doesn't block others since each send happens independently.
func (w *Worker) processDue(ctx context.Context) {
	rows, err := w.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM delivery_log
			WHERE status IN ('pending', 'retrying') AND (next_attempt_at IS NULL OR next_attempt_at <= now())
			ORDER BY created_at
			LIMIT 50
			FOR UPDATE SKIP LOCKED
		)
		UPDATE delivery_log d
		SET last_attempt_at = now()
		FROM due
		WHERE d.id = due.id
		RETURNING d.id, d.rule_id, d.notification_target_id, d.event_type, d.attempt_count, d.max_attempts, d.payload`)
	if err != nil {
		w.logger.Error("claiming due deliveries", "error", err)
		return
	}
	var claimed []claimedDelivery
	for rows.Next() {
		var c claimedDelivery
		if err := rows.Scan(&c.id, &c.ruleID, &c.notificationTargetID, &c.eventType, &c.attemptCount, &c.maxAttempts, &c.payload); err != nil {
			w.logger.Error("scanning claimed delivery", "error", err)
			continue
		}
		claimed = append(claimed, c)
	}
	rows.Close()

	for _, c := range claimed {
		w.attempt(ctx, c)
	}
}

func (w *Worker) attempt(ctx context.Context, c claimedDelivery) {
	target, err := w.notifications.Get(ctx, c.notificationTargetID)
	if err != nil {
		w.fail(ctx, c, 0, fmt.Sprintf("looking up notification target: %v", err))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.WebhookURL, bytes.NewReader(c.payload))
	if err != nil {
		w.fail(ctx, c, 0, fmt.Sprintf("building request: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.http.Do(req)
	if err != nil {
		w.fail(ctx, c, 0, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.markSent(ctx, c.id, resp.StatusCode)
		return
	}
	w.fail(ctx, c, resp.StatusCode, fmt.Sprintf("non-2xx response: %d", resp.StatusCode))
}

func (w *Worker) markSent(ctx context.Context, id int64, statusCode int) {
	_, err := w.pool.Exec(ctx, `
		UPDATE delivery_log SET status = 'sent', attempt_count = attempt_count + 1, response_status = $1 WHERE id = $2`,
		statusCode, id)
	if err != nil {
		w.logger.Error("marking delivery sent", "delivery_id", id, "error", err)
	}
}

// fail records a failed attempt. If attempts remain, it's rescheduled
// with exponential backoff (2^attempt_count seconds, capped at 1 hour);
// otherwise it's marked permanently failed.
func (w *Worker) fail(ctx context.Context, c claimedDelivery, statusCode int, errMsg string) {
	nextAttempt := c.attemptCount + 1
	if nextAttempt >= c.maxAttempts {
		_, err := w.pool.Exec(ctx, `
			UPDATE delivery_log SET status = 'failed', attempt_count = attempt_count + 1, response_status = $1, last_error = $2 WHERE id = $3`,
			nullIfZero(statusCode), errMsg, c.id)
		if err != nil {
			w.logger.Error("marking delivery failed", "delivery_id", c.id, "error", err)
		}
		return
	}

	backoff := time.Duration(1<<uint(nextAttempt)) * time.Second
	if backoff > time.Hour {
		backoff = time.Hour
	}
	_, err := w.pool.Exec(ctx, `
		UPDATE delivery_log
		SET status = 'retrying', attempt_count = attempt_count + 1, response_status = $1, last_error = $2, next_attempt_at = now() + $3
		WHERE id = $4`,
		nullIfZero(statusCode), errMsg, backoff, c.id)
	if err != nil {
		w.logger.Error("scheduling delivery retry", "delivery_id", c.id, "error", err)
	}
}

func nullIfZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
