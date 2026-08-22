package evaluator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/alerting/internal/delivery"
	"github.com/cairnobs/cairnobs/alerting/internal/notifystore"
	"github.com/cairnobs/cairnobs/alerting/internal/queryclient"
	"github.com/cairnobs/cairnobs/alerting/internal/rulestore"
)

// Evaluator is the ticker-driven scheduler -- a bounded worker pool, not
// a workflow engine, per the design doc's explicit instruction. Each
// tick claims up to claimBatchSize due rules (rulestore.ClaimDueRules,
// fix 1's atomic claim) and evaluates them concurrently up to
// workerPoolSize at a time. These are deliberately different numbers --
// see config.EvaluatorConfig's doc comment for the real bug this fixes
// (500 rules due at once, both capped at 20, took 125s to cycle through
// instead of the configured 60s).
type Evaluator struct {
	rules          *rulestore.Store
	notifications  *notifystore.Store
	queryClient    *queryclient.Client
	queryTimeout   time.Duration
	claimBatchSize int
	workerPoolSize int
	logger         *slog.Logger
}

func New(rules *rulestore.Store, notifications *notifystore.Store, queryClient *queryclient.Client, queryTimeout time.Duration, claimBatchSize, workerPoolSize int, logger *slog.Logger) *Evaluator {
	return &Evaluator{
		rules: rules, notifications: notifications, queryClient: queryClient,
		queryTimeout: queryTimeout, claimBatchSize: claimBatchSize, workerPoolSize: workerPoolSize, logger: logger,
	}
}

// Run ticks every tickInterval until ctx is cancelled.
func (e *Evaluator) Run(ctx context.Context, tickInterval time.Duration) error {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

func (e *Evaluator) tick(ctx context.Context) {
	claimed, err := e.rules.ClaimDueRules(ctx, e.claimBatchSize)
	if err != nil {
		e.logger.Error("claiming due rules", "error", err)
		return
	}
	if len(claimed) == 0 {
		return
	}

	sem := make(chan struct{}, e.workerPoolSize)
	var wg sync.WaitGroup
	for _, rule := range claimed {
		wg.Add(1)
		sem <- struct{}{}
		go func(rule rulestore.RuleWithState) {
			defer wg.Done()
			defer func() { <-sem }()
			e.evaluateOne(ctx, rule)
		}(rule)
	}
	wg.Wait()
}

func (e *Evaluator) evaluateOne(ctx context.Context, rule rulestore.RuleWithState) {
	result, err := e.queryClient.Query(ctx, rule.Query, rule.QueryLanguage, e.queryTimeout)
	if err != nil {
		// A failed /query call is an evaluation error, never
		// "condition false" -- fix 3. Recording it here, not routing it
		// through ComputeTransition at all, is what makes that guarantee
		// hold structurally rather than by convention.
		e.recordError(ctx, rule.ID, err.Error())
		return
	}

	conditionTrue, value, evalErr := evaluateCondition(rule.Rule, result)
	if evalErr != nil {
		e.recordError(ctx, rule.ID, evalErr.Error())
		return
	}

	var renotify *time.Duration
	if rule.RenotifyIntervalMinutes != nil {
		d := time.Duration(*rule.RenotifyIntervalMinutes) * time.Minute
		renotify = &d
	}

	now := time.Now().UTC()
	transition := ComputeTransition(TransitionInput{
		CurrentState:       rule.State.State,
		ConditionTrueSince: rule.State.ConditionTrueSince,
		FiredAt:            rule.State.FiredAt,
		LastNotifiedAt:     rule.State.LastNotifiedAt,
		ForMinutes:         time.Duration(rule.ForMinutes) * time.Minute,
		RenotifyInterval:   renotify,
		Now:                now,
		ConditionTrue:      conditionTrue,
	})

	next := rulestore.AlertState{
		State:              transition.NextState,
		ConditionTrueSince: transition.NextConditionTrueSince,
		FiredAt:            transition.NextFiredAt,
		LastNotifiedAt:     transition.NextLastNotifiedAt,
		LastValue:          value,
	}

	notify := e.buildNotifyEvent(ctx, rule, transition, value, now)

	if err := e.rules.ApplyTransition(ctx, rule.ID, next, notify); err != nil {
		e.logger.Error("applying alert state transition", "rule_id", rule.ID, "error", err)
	}
}

// buildNotifyEvent resolves the notification target and renders the
// payload for a firing/resolved transition. A lookup or template
// failure here is logged and treated as "no notification this time" --
// the state still transitions correctly (that's the more important
// guarantee), it just means a misconfigured target/template silently
// drops one notification rather than blocking the whole evaluation.
func (e *Evaluator) buildNotifyEvent(ctx context.Context, rule rulestore.RuleWithState, transition TransitionResult, value *float64, now time.Time) *rulestore.NotifyEvent {
	if transition.Notify == nil {
		return nil
	}

	target, err := e.notifications.Get(ctx, rule.NotificationTargetID)
	if err != nil {
		e.logger.Error("looking up notification target", "rule_id", rule.ID, "target_id", rule.NotificationTargetID, "error", err)
		return nil
	}

	comparator := ""
	if rule.Comparator != nil {
		comparator = string(*rule.Comparator)
	}
	payload, err := delivery.BuildPayload(*target, delivery.Event{
		RuleID: rule.ID, RuleName: rule.Name, EventType: *transition.Notify,
		ConditionType: string(rule.ConditionType), Comparator: comparator,
		ThresholdValue: rule.ThresholdValue, Value: value, Timestamp: now,
	})
	if err != nil {
		e.logger.Error("building notification payload", "rule_id", rule.ID, "error", err)
		return nil
	}

	return &rulestore.NotifyEvent{
		NotificationTargetID: rule.NotificationTargetID,
		EventType:            *transition.Notify,
		Payload:              payload,
	}
}

func (e *Evaluator) recordError(ctx context.Context, ruleID, msg string) {
	if err := e.rules.RecordError(ctx, ruleID, msg); err != nil {
		e.logger.Error("recording evaluation error", "rule_id", ruleID, "error", err)
	}
}
