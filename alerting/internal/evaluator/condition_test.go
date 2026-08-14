package evaluator

import (
	"testing"

	"github.com/sentry/sentry/alerting/internal/queryclient"
	"github.com/sentry/sentry/alerting/internal/rulestore"
)

func thresholdRule(comparator rulestore.Comparator, threshold float64) rulestore.Rule {
	return rulestore.Rule{
		ConditionType:  rulestore.ConditionThreshold,
		Comparator:     &comparator,
		ThresholdValue: &threshold,
	}
}

func TestEvaluateConditionThresholdTrue(t *testing.T) {
	rule := thresholdRule(rulestore.Gt, 100)
	result := &queryclient.Result{Columns: []string{"count"}, Rows: [][]any{{150.0}}}

	got, value, err := evaluateCondition(rule, result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("expected condition true for 150 > 100")
	}
	if value == nil || *value != 150.0 {
		t.Fatalf("expected value=150, got %v", value)
	}
}

func TestEvaluateConditionThresholdFalse(t *testing.T) {
	rule := thresholdRule(rulestore.Gt, 100)
	result := &queryclient.Result{Columns: []string{"count"}, Rows: [][]any{{50.0}}}

	got, _, err := evaluateCondition(rule, result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("expected condition false for 50 > 100")
	}
}

// TestEvaluateConditionThresholdZeroRowsIsError pins down fix 4: zero
// rows on a threshold rule must be an error, not silently coerced to 0
// (which would make `count > 100` falsely report "fine" when actually
// nothing ran).
func TestEvaluateConditionThresholdZeroRowsIsError(t *testing.T) {
	rule := thresholdRule(rulestore.Gt, 100)
	result := &queryclient.Result{Columns: []string{"count"}, Rows: [][]any{}}

	_, value, err := evaluateCondition(rule, result)
	if err == nil {
		t.Fatalf("expected an error for zero rows on a threshold rule, got condition evaluated with value=%v", value)
	}
}

func TestEvaluateConditionThresholdMultipleRowsIsError(t *testing.T) {
	rule := thresholdRule(rulestore.Gt, 100)
	result := &queryclient.Result{Columns: []string{"host", "count"}, Rows: [][]any{{"h1", 50.0}, {"h2", 200.0}}}

	_, _, err := evaluateCondition(rule, result)
	if err == nil {
		t.Fatalf("expected an error for multiple rows on a threshold rule (no per-group alerting, a named non-goal)")
	}
}

func TestEvaluateConditionThresholdNonNumericIsError(t *testing.T) {
	rule := thresholdRule(rulestore.Gt, 100)
	result := &queryclient.Result{Columns: []string{"host"}, Rows: [][]any{{"host-01"}}}

	_, _, err := evaluateCondition(rule, result)
	if err == nil {
		t.Fatalf("expected an error for a non-numeric first column")
	}
}

func TestEvaluateConditionAbsenceTrueWhenZeroRows(t *testing.T) {
	rule := rulestore.Rule{ConditionType: rulestore.ConditionAbsence}
	result := &queryclient.Result{Columns: []string{"message"}, Rows: [][]any{}}

	got, value, err := evaluateCondition(rule, result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatalf("expected absence condition true for zero rows")
	}
	if value != nil {
		t.Fatalf("expected nil value for an absence rule, got %v", value)
	}
}

func TestEvaluateConditionAbsenceFalseWhenRowsPresent(t *testing.T) {
	rule := rulestore.Rule{ConditionType: rulestore.ConditionAbsence}
	result := &queryclient.Result{Columns: []string{"message"}, Rows: [][]any{{"something happened"}}}

	got, _, err := evaluateCondition(rule, result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatalf("expected absence condition false when rows are present")
	}
}

func TestEvaluateConditionUnknownTypeIsError(t *testing.T) {
	rule := rulestore.Rule{ConditionType: "bogus"}
	result := &queryclient.Result{Columns: []string{"count"}, Rows: [][]any{{1.0}}}

	_, _, err := evaluateCondition(rule, result)
	if err == nil {
		t.Fatalf("expected an error for an unknown condition_type")
	}
}
