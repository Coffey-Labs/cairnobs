package evaluator

import (
	"fmt"

	"github.com/cairnobs/cairnobs/alerting/internal/queryclient"
	"github.com/cairnobs/cairnobs/alerting/internal/rulestore"
)

// evaluateCondition implements fixes 3 and 4 from
// /docs/phase-3-alerting-design.md: a threshold rule's query must
// resolve to exactly one row, and zero (or more than one) rows is
// returned as an error, never coerced to a value -- "nothing ran" and
// "ran and found nothing" are different failure/success modes, and
// conflating them can hide the more alarming case. This function never
// itself decides "condition false" on an error; it returns an error, and
// the caller (evaluator.go) routes that to rulestore.RecordError, never
// to ComputeTransition.
func evaluateCondition(rule rulestore.Rule, result *queryclient.Result) (conditionTrue bool, value *float64, err error) {
	switch rule.ConditionType {
	case rulestore.ConditionAbsence:
		// The window is whatever earliest=/latest= the rule's own query
		// already expresses -- no separate window field, per the design doc.
		return len(result.Rows) == 0, nil, nil

	case rulestore.ConditionThreshold:
		if len(result.Rows) != 1 {
			return false, nil, fmt.Errorf("threshold rule query returned %d rows, want exactly 1", len(result.Rows))
		}
		row := result.Rows[0]
		if len(row) == 0 {
			return false, nil, fmt.Errorf("threshold rule query returned a row with no columns")
		}
		v, ok := toFloat64(row[0])
		if !ok {
			return false, nil, fmt.Errorf("threshold rule's first column value %v is not numeric", row[0])
		}
		if rule.Comparator == nil || rule.ThresholdValue == nil {
			return false, nil, fmt.Errorf("threshold rule is missing comparator or threshold_value")
		}
		return compare(v, *rule.Comparator, *rule.ThresholdValue), &v, nil

	default:
		return false, nil, fmt.Errorf("unknown condition_type %q", rule.ConditionType)
	}
}

func compare(value float64, comparator rulestore.Comparator, threshold float64) bool {
	switch comparator {
	case rulestore.Gt:
		return value > threshold
	case rulestore.Gte:
		return value >= threshold
	case rulestore.Lt:
		return value < threshold
	case rulestore.Lte:
		return value <= threshold
	case rulestore.Eq:
		return value == threshold
	case rulestore.Ne:
		return value != threshold
	default:
		return false
	}
}

// toFloat64 handles the one shape /query responses actually come back
// as: Go's encoding/json always decodes JSON numbers into interface{}
// as float64, regardless of whether ClickHouse serialized an integer or
// a float -- there's no separate int64 case to handle.
func toFloat64(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
