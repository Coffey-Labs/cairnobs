// Package rulestore is pgx-backed CRUD for alert_rules/alert_state, plus
// the two operations that make the evaluator's concurrency and delivery
// guarantees hold (see /docs/phase-3-alerting-design.md's fixes 1 and 2):
// ClaimDueRules (atomic claim-before-evaluate) and ApplyTransition (the
// state transition and the delivery_log outbox insert in one DB
// transaction).
package rulestore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type ConditionType string

const (
	ConditionThreshold ConditionType = "threshold"
	ConditionAbsence   ConditionType = "absence"
)

func ValidConditionType(c ConditionType) bool {
	return c == ConditionThreshold || c == ConditionAbsence
}

type Comparator string

const (
	Gt  Comparator = "gt"
	Gte Comparator = "gte"
	Lt  Comparator = "lt"
	Lte Comparator = "lte"
	Eq  Comparator = "eq"
	Ne  Comparator = "ne"
)

func ValidComparator(c Comparator) bool {
	switch c {
	case Gt, Gte, Lt, Lte, Eq, Ne:
		return true
	default:
		return false
	}
}

type State string

const (
	StateOK      State = "ok"
	StatePending State = "pending"
	StateFiring  State = "firing"
)

type Rule struct {
	ID                      string        `json:"id"`
	TenantID                string        `json:"tenant_id"`
	Name                    string        `json:"name"`
	Description             string        `json:"description"`
	Query                   string        `json:"query"`
	QueryLanguage           string        `json:"query_language"`
	ConditionType           ConditionType `json:"condition_type"`
	Comparator              *Comparator   `json:"comparator,omitempty"`
	ThresholdValue          *float64      `json:"threshold_value,omitempty"`
	EvalIntervalSeconds     int           `json:"eval_interval_seconds"`
	ForMinutes              int           `json:"for_minutes"`
	RenotifyIntervalMinutes *int          `json:"renotify_interval_minutes,omitempty"`
	NotificationTargetID    string        `json:"notification_target_id"`
	Enabled                 bool          `json:"enabled"`
	CreatedBy               string        `json:"created_by"`
}

type AlertState struct {
	RuleID             string     `json:"rule_id"`
	State              State      `json:"state"`
	ConditionTrueSince *time.Time `json:"condition_true_since,omitempty"`
	FiredAt            *time.Time `json:"fired_at,omitempty"`
	LastNotifiedAt     *time.Time `json:"last_notified_at,omitempty"`
	LastEvaluatedAt    *time.Time `json:"last_evaluated_at,omitempty"`
	LastEvalStatus     string     `json:"last_eval_status"`
	LastError          *string    `json:"last_error,omitempty"`
	LastValue          *float64   `json:"last_value,omitempty"`
	ConsecutiveErrors  int        `json:"consecutive_errors"`
}

// RuleWithState is a rule joined with its current live state, exactly
// what the evaluator needs to run one evaluation and what the read API
// returns.
type RuleWithState struct {
	Rule
	State AlertState `json:"state"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts the rule and its initial alert_state row in one
// transaction. A rule with no alert_state row is silently never picked
// up by ClaimDueRules -- see the design doc's explicit warning about
// this exact failure mode.
func (s *Store) Create(ctx context.Context, r *Rule) error {
	r.ID = uuid.NewString()
	if r.TenantID == "" {
		r.TenantID = "default"
	}
	if r.CreatedBy == "" {
		r.CreatedBy = "anonymous"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO alert_rules (id, tenant_id, name, description, query, query_language, condition_type,
		                          comparator, threshold_value, eval_interval_seconds, for_minutes,
		                          renotify_interval_minutes, notification_target_id, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		r.ID, r.TenantID, r.Name, r.Description, r.Query, r.QueryLanguage, r.ConditionType,
		r.Comparator, r.ThresholdValue, r.EvalIntervalSeconds, r.ForMinutes,
		r.RenotifyIntervalMinutes, r.NotificationTargetID, r.Enabled, r.CreatedBy)
	if err != nil {
		return fmt.Errorf("inserting rule: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO alert_state (rule_id, state, last_eval_status, next_eval_at)
		VALUES ($1, 'ok', 'ok', now())`, r.ID)
	if err != nil {
		return fmt.Errorf("inserting initial alert_state: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *Store) List(ctx context.Context) ([]RuleWithState, error) {
	rows, err := s.pool.Query(ctx, ruleWithStateSelect+" ORDER BY r.created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RuleWithState
	for rows.Next() {
		rs, err := scanRuleWithState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (*RuleWithState, error) {
	rows, err := s.pool.Query(ctx, ruleWithStateSelect+" WHERE r.id = $1", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, ErrNotFound
	}
	rs, err := scanRuleWithState(rows)
	if err != nil {
		return nil, err
	}
	return &rs, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const ruleWithStateSelect = `
	SELECT r.id, r.tenant_id, r.name, r.description, r.query, r.query_language, r.condition_type,
	       r.comparator, r.threshold_value, r.eval_interval_seconds, r.for_minutes,
	       r.renotify_interval_minutes, r.notification_target_id, r.enabled, r.created_by,
	       s.state, s.condition_true_since, s.fired_at, s.last_notified_at, s.last_evaluated_at,
	       s.last_eval_status, s.last_error, s.last_value, s.consecutive_errors
	FROM alert_rules r JOIN alert_state s ON s.rule_id = r.id`

func scanRuleWithState(rows pgx.Rows) (RuleWithState, error) {
	var rs RuleWithState
	err := rows.Scan(
		&rs.ID, &rs.TenantID, &rs.Name, &rs.Description, &rs.Query, &rs.QueryLanguage, &rs.ConditionType,
		&rs.Comparator, &rs.ThresholdValue, &rs.EvalIntervalSeconds, &rs.ForMinutes,
		&rs.RenotifyIntervalMinutes, &rs.NotificationTargetID, &rs.Enabled, &rs.CreatedBy,
		&rs.State.State, &rs.State.ConditionTrueSince, &rs.State.FiredAt, &rs.State.LastNotifiedAt, &rs.State.LastEvaluatedAt,
		&rs.State.LastEvalStatus, &rs.State.LastError, &rs.State.LastValue, &rs.State.ConsecutiveErrors)
	rs.State.RuleID = rs.ID
	return rs, err
}

// ClaimDueRules atomically claims up to batchSize due, enabled rules --
// see /docs/phase-3-alerting-design.md's fix 1. next_eval_at is bumped
// forward *before* returning, so a second scheduler tick (or a second
// evaluator replica, later) can't re-select the same rule while this
// one is still being evaluated. SKIP LOCKED means a concurrent claim
// query never blocks on rows another claim already has locked -- it
// just moves on to the next candidate.
func (s *Store) ClaimDueRules(ctx context.Context, batchSize int) ([]RuleWithState, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT s.rule_id
			FROM alert_state s
			JOIN alert_rules r ON r.id = s.rule_id
			WHERE s.next_eval_at <= now() AND r.enabled
			ORDER BY s.next_eval_at
			LIMIT $1
			FOR UPDATE OF s SKIP LOCKED
		)
		UPDATE alert_state s
		SET next_eval_at = now() + make_interval(secs => r.eval_interval_seconds),
		    claimed_at = now()
		FROM due, alert_rules r
		WHERE s.rule_id = due.rule_id AND r.id = s.rule_id
		RETURNING r.id, r.tenant_id, r.name, r.description, r.query, r.query_language, r.condition_type,
		          r.comparator, r.threshold_value, r.eval_interval_seconds, r.for_minutes,
		          r.renotify_interval_minutes, r.notification_target_id, r.enabled, r.created_by,
		          s.state, s.condition_true_since, s.fired_at, s.last_notified_at, s.last_evaluated_at,
		          s.last_eval_status, s.last_error, s.last_value, s.consecutive_errors`,
		batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RuleWithState
	for rows.Next() {
		rs, err := scanRuleWithState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// NotifyEvent, if non-nil, is inserted into delivery_log in the same
// transaction as the state update -- the transactional outbox from fix
// 2. Payload is the already-rendered notification body (rulestore
// doesn't know how to format one; that's internal/delivery's job).
type NotifyEvent struct {
	NotificationTargetID string
	EventType            string // "firing" | "resolved"
	Payload              []byte // JSON
}

// ApplyTransition writes a successful evaluation's outcome: the new
// alert_state fields, and -- atomically, in the same transaction -- a
// pending delivery_log row if notify is non-nil. See fix 2: "decided to
// notify" becomes durable exactly once, in lockstep with the state
// change, before any network call to a notification target is
// attempted.
func (s *Store) ApplyTransition(ctx context.Context, ruleID string, next AlertState, notify *NotifyEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE alert_state SET
			state = $1, condition_true_since = $2, fired_at = $3, last_notified_at = $4,
			last_evaluated_at = now(), last_eval_status = 'ok', last_error = NULL,
			last_value = $5, consecutive_errors = 0
		WHERE rule_id = $6`,
		next.State, next.ConditionTrueSince, next.FiredAt, next.LastNotifiedAt, next.LastValue, ruleID)
	if err != nil {
		return fmt.Errorf("updating alert_state: %w", err)
	}

	if notify != nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO delivery_log (rule_id, notification_target_id, event_type, status, next_attempt_at, payload)
			VALUES ($1, $2, $3, 'pending', now(), $4)`,
			ruleID, notify.NotificationTargetID, notify.EventType, notify.Payload)
		if err != nil {
			return fmt.Errorf("inserting delivery_log outbox row: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// DeliveryLogEntry is the read shape the web UI's delivery log view and
// the httpapi read endpoint use -- deliberately excludes `payload`
// (the rendered notification body isn't needed for the list view, and
// keeping it out of the default read path avoids echoing
// target-specific formatting, e.g. a rendered webhook secret, back over
// HTTP by default).
type DeliveryLogEntry struct {
	ID                   int64     `json:"id"`
	RuleID               string    `json:"rule_id"`
	NotificationTargetID string    `json:"notification_target_id"`
	EventType            string    `json:"event_type"`
	Status               string    `json:"status"`
	AttemptCount         int       `json:"attempt_count"`
	LastError            *string   `json:"last_error,omitempty"`
	ResponseStatus       *int      `json:"response_status,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// ListForRule reads delivery_log's UI-visible role -- "why didn't I get
// paged" -- most recent first.
func (s *Store) ListForRule(ctx context.Context, ruleID string, limit int) ([]DeliveryLogEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, rule_id, notification_target_id, event_type, status, attempt_count, last_error, response_status, created_at
		FROM delivery_log WHERE rule_id = $1 ORDER BY created_at DESC LIMIT $2`, ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeliveryLogEntry
	for rows.Next() {
		var e DeliveryLogEntry
		if err := rows.Scan(&e.ID, &e.RuleID, &e.NotificationTargetID, &e.EventType, &e.Status, &e.AttemptCount, &e.LastError, &e.ResponseStatus, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordError updates only the error-tracking fields on alert_state --
// state itself is left untouched, per fix 3: an evaluation error must
// never transition state (an error is not "condition false").
func (s *Store) RecordError(ctx context.Context, ruleID, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE alert_state SET
			last_evaluated_at = now(), last_eval_status = 'error', last_error = $1,
			consecutive_errors = consecutive_errors + 1
		WHERE rule_id = $2`, errMsg, ruleID)
	return err
}
