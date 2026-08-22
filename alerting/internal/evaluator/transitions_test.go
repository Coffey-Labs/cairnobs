package evaluator

import (
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/alerting/internal/rulestore"
)

var t0 = time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func TestComputeTransition(t *testing.T) {
	tests := []struct {
		name string
		in   TransitionInput
		want TransitionResult
	}{
		{
			name: "ok, condition true, for_minutes>0 -> pending, no notify",
			in: TransitionInput{
				CurrentState: rulestore.StateOK, ForMinutes: 5 * time.Minute,
				Now: t0, ConditionTrue: true,
			},
			want: TransitionResult{NextState: rulestore.StatePending, NextConditionTrueSince: ptr(t0)},
		},
		{
			name: "ok, condition true, for_minutes=0 -> fires immediately, no extra tick in pending",
			in: TransitionInput{
				CurrentState: rulestore.StateOK, ForMinutes: 0,
				Now: t0, ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				NextFiredAt: ptr(t0), NextLastNotifiedAt: ptr(t0), Notify: ptr("firing"),
			},
		},
		{
			name: "pending, still within for_minutes window -> stays pending, no notify",
			in: TransitionInput{
				CurrentState: rulestore.StatePending, ConditionTrueSince: ptr(t0),
				ForMinutes: 5 * time.Minute, Now: t0.Add(2 * time.Minute), ConditionTrue: true,
			},
			want: TransitionResult{NextState: rulestore.StatePending, NextConditionTrueSince: ptr(t0)},
		},
		{
			name: "pending, exactly at for_minutes boundary -> fires (>=, not >)",
			in: TransitionInput{
				CurrentState: rulestore.StatePending, ConditionTrueSince: ptr(t0),
				ForMinutes: 5 * time.Minute, Now: t0.Add(5 * time.Minute), ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				NextFiredAt: ptr(t0.Add(5 * time.Minute)), NextLastNotifiedAt: ptr(t0.Add(5 * time.Minute)), Notify: ptr("firing"),
			},
		},
		{
			name: "pending, past for_minutes -> fires",
			in: TransitionInput{
				CurrentState: rulestore.StatePending, ConditionTrueSince: ptr(t0),
				ForMinutes: 5 * time.Minute, Now: t0.Add(10 * time.Minute), ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				NextFiredAt: ptr(t0.Add(10 * time.Minute)), NextLastNotifiedAt: ptr(t0.Add(10 * time.Minute)), Notify: ptr("firing"),
			},
		},
		{
			name: "firing, condition still true, no renotify interval -> stays firing, silent, preserves fired_at/last_notified",
			in: TransitionInput{
				CurrentState: rulestore.StateFiring, ConditionTrueSince: ptr(t0), FiredAt: ptr(t0), LastNotifiedAt: ptr(t0),
				RenotifyInterval: nil, Now: t0.Add(time.Hour), ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				NextFiredAt: ptr(t0), NextLastNotifiedAt: ptr(t0), Notify: nil,
			},
		},
		{
			name: "firing, condition still true, renotify interval not yet elapsed -> stays firing, silent",
			in: TransitionInput{
				CurrentState: rulestore.StateFiring, ConditionTrueSince: ptr(t0), FiredAt: ptr(t0), LastNotifiedAt: ptr(t0),
				RenotifyInterval: ptr(4 * time.Hour), Now: t0.Add(time.Hour), ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				NextFiredAt: ptr(t0), NextLastNotifiedAt: ptr(t0), Notify: nil,
			},
		},
		{
			name: "firing, condition still true, renotify interval elapsed -> re-fires, updates last_notified only",
			in: TransitionInput{
				CurrentState: rulestore.StateFiring, ConditionTrueSince: ptr(t0), FiredAt: ptr(t0), LastNotifiedAt: ptr(t0),
				RenotifyInterval: ptr(4 * time.Hour), Now: t0.Add(5 * time.Hour), ConditionTrue: true,
			},
			want: TransitionResult{
				NextState: rulestore.StateFiring, NextConditionTrueSince: ptr(t0),
				// fired_at (the episode start) is NOT bumped on a renotify -- only last_notified_at moves.
				NextFiredAt: ptr(t0), NextLastNotifiedAt: ptr(t0.Add(5 * time.Hour)), Notify: ptr("firing"),
			},
		},
		{
			name: "ok, condition false -> stays ok, no-op",
			in:   TransitionInput{CurrentState: rulestore.StateOK, Now: t0, ConditionTrue: false},
			want: TransitionResult{NextState: rulestore.StateOK},
		},
		{
			name: "pending, condition false -> back to ok, NO notification (a blip inside the debounce window)",
			in: TransitionInput{
				CurrentState: rulestore.StatePending, ConditionTrueSince: ptr(t0),
				Now: t0.Add(time.Minute), ConditionTrue: false,
			},
			want: TransitionResult{NextState: rulestore.StateOK},
		},
		{
			name: "firing, condition false -> resolves, sends resolved notification",
			in: TransitionInput{
				CurrentState: rulestore.StateFiring, ConditionTrueSince: ptr(t0), FiredAt: ptr(t0), LastNotifiedAt: ptr(t0),
				Now: t0.Add(time.Hour), ConditionTrue: false,
			},
			want: TransitionResult{NextState: rulestore.StateOK, Notify: ptr("resolved")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeTransition(tt.in)
			if got.NextState != tt.want.NextState {
				t.Errorf("NextState = %v, want %v", got.NextState, tt.want.NextState)
			}
			if !timePtrEqual(got.NextConditionTrueSince, tt.want.NextConditionTrueSince) {
				t.Errorf("NextConditionTrueSince = %v, want %v", got.NextConditionTrueSince, tt.want.NextConditionTrueSince)
			}
			if !timePtrEqual(got.NextFiredAt, tt.want.NextFiredAt) {
				t.Errorf("NextFiredAt = %v, want %v", got.NextFiredAt, tt.want.NextFiredAt)
			}
			if !timePtrEqual(got.NextLastNotifiedAt, tt.want.NextLastNotifiedAt) {
				t.Errorf("NextLastNotifiedAt = %v, want %v", got.NextLastNotifiedAt, tt.want.NextLastNotifiedAt)
			}
			if !strPtrEqual(got.Notify, tt.want.Notify) {
				t.Errorf("Notify = %v, want %v", strPtrDeref(got.Notify), strPtrDeref(tt.want.Notify))
			}
		})
	}
}

// TestPendingConditionTrueSinceIsWallClockNotCounter pins down the
// specific property the design doc calls out as easy to accidentally
// "simplify" away: debounce progress survives an evaluator gap (e.g.
// downtime) because condition_true_since is a timestamp, not a tick
// count. Two evaluations three hours apart, both condition=true, with a
// for_minutes=5m debounce -- the second evaluation must fire immediately
// because wall-clock time already satisfies the debounce, regardless of
// how many (or how few) evaluations happened in between.
func TestPendingConditionTrueSinceIsWallClockNotCounter(t *testing.T) {
	first := ComputeTransition(TransitionInput{
		CurrentState: rulestore.StateOK, ForMinutes: 5 * time.Minute, Now: t0, ConditionTrue: true,
	})
	if first.NextState != rulestore.StatePending {
		t.Fatalf("first eval: state = %v, want pending", first.NextState)
	}

	// Simulate a large gap (evaluator downtime) before the next evaluation.
	second := ComputeTransition(TransitionInput{
		CurrentState: rulestore.StatePending, ConditionTrueSince: first.NextConditionTrueSince,
		ForMinutes: 5 * time.Minute, Now: t0.Add(3 * time.Hour), ConditionTrue: true,
	})
	if second.NextState != rulestore.StateFiring {
		t.Fatalf("second eval after gap: state = %v, want firing (wall-clock debounce should already be satisfied)", second.NextState)
	}
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strPtrDeref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
