// Package evaluator implements the ticker-driven rule scheduler and the
// firing/resolved state machine described in
// /docs/phase-3-alerting-design.md. transitions.go is deliberately pure
// (no DB, no HTTP, no clock reads beyond the `Now` passed in) --
// evaluator.go is the only caller, and it's the single place worth
// exhaustive table-driven testing given how easy this state machine is
// to get subtly wrong.
package evaluator

import (
	"time"

	"github.com/cairnobs/cairnobs/alerting/internal/rulestore"
)

// TransitionInput is everything ComputeTransition needs: the rule's
// current persisted state plus this evaluation's outcome. Evaluation
// *errors* never reach this function at all -- per fix 3 in the design
// doc, an error is recorded separately (rulestore.RecordError) and never
// treated as ConditionTrue: false.
type TransitionInput struct {
	CurrentState       rulestore.State
	ConditionTrueSince *time.Time
	FiredAt            *time.Time
	LastNotifiedAt     *time.Time
	ForMinutes         time.Duration
	RenotifyInterval   *time.Duration // nil = notify once per firing episode, never again
	Now                time.Time
	ConditionTrue      bool
}

// TransitionResult is what to persist (via rulestore.ApplyTransition)
// and, if Notify is non-nil, which event ("firing" or "resolved") to
// enqueue in the same transaction.
type TransitionResult struct {
	NextState              rulestore.State
	NextConditionTrueSince *time.Time
	NextFiredAt            *time.Time
	NextLastNotifiedAt     *time.Time
	Notify                 *string
}

// ComputeTransition implements the ok/pending/firing state machine.
// ConditionTrueSince is always a wall-clock timestamp, never a
// consecutive-evaluation counter -- this is what makes the for_minutes
// debounce survive evaluator restarts correctly (a counter would
// silently lose progress across downtime; wall-clock math resumes
// exactly where it left off). Don't "simplify" this into a counter.
func ComputeTransition(in TransitionInput) TransitionResult {
	if in.ConditionTrue {
		return computeConditionTrue(in)
	}
	return computeConditionFalse(in)
}

func computeConditionTrue(in TransitionInput) TransitionResult {
	switch in.CurrentState {
	case rulestore.StatePending:
		since := in.ConditionTrueSince
		if since == nil {
			// Defensive: pending with no recorded since is inconsistent
			// persisted state (shouldn't happen -- ok->pending always sets
			// it) -- treat as if the condition just became true rather than
			// dereferencing a nil or panicking.
			now := in.Now
			since = &now
		}
		if in.Now.Sub(*since) >= in.ForMinutes {
			return fire(*since, in.Now)
		}
		return TransitionResult{NextState: rulestore.StatePending, NextConditionTrueSince: since}

	case rulestore.StateFiring:
		result := TransitionResult{
			NextState:              rulestore.StateFiring,
			NextConditionTrueSince: in.ConditionTrueSince,
			NextFiredAt:            in.FiredAt,
			NextLastNotifiedAt:     in.LastNotifiedAt,
		}
		if in.RenotifyInterval != nil && in.LastNotifiedAt != nil && in.Now.Sub(*in.LastNotifiedAt) >= *in.RenotifyInterval {
			now := in.Now
			event := "firing"
			result.NextLastNotifiedAt = &now
			result.Notify = &event
		}
		return result

	default: // ok (or any unexpected value -- the DB CHECK constraint keeps this to 3 values)
		since := in.Now
		if in.ForMinutes <= 0 {
			// for_minutes=0 means "fire on first true evaluation" --
			// literally, not after one extra tick spent in `pending`.
			return fire(since, in.Now)
		}
		return TransitionResult{NextState: rulestore.StatePending, NextConditionTrueSince: &since}
	}
}

func computeConditionFalse(in TransitionInput) TransitionResult {
	switch in.CurrentState {
	case rulestore.StateFiring:
		event := "resolved"
		return TransitionResult{NextState: rulestore.StateOK, Notify: &event}
	default: // ok or pending: a false evaluation while pending is a blip
		// inside the debounce window -- deliberately no notification, that's
		// the entire point of for_minutes existing.
		return TransitionResult{NextState: rulestore.StateOK}
	}
}

func fire(since, now time.Time) TransitionResult {
	firedAt := now
	event := "firing"
	return TransitionResult{
		NextState:              rulestore.StateFiring,
		NextConditionTrueSince: &since,
		NextFiredAt:            &firedAt,
		NextLastNotifiedAt:     &firedAt,
		Notify:                 &event,
	}
}
