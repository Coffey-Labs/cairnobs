package costguard

import (
	"testing"
	"time"

	"github.com/sentry/sentry/api/internal/querylang/ir"
)

// A raw-row (non-aggregation) query with no time range and no explicit
// Limit still gets executor/sql.go's defaultRowLimit=100 safety net
// applied automatically -- so this is a Warn (a possibly-expensive scan
// to find those 100 rows), not a Reject (genuinely unbounded output),
// which only an unbounded *aggregation* actually is. See the case
// immediately below for that contrast.
func TestAssessNoTimeRangeNoLimitRawRowWarns(t *testing.T) {
	plan := &ir.Plan{Filters: []ir.FilterPredicate{{Field: "service", Op: "=", Value: "api"}}}
	got := Assess(plan)
	if got.Level != LevelWarn {
		t.Errorf("Level = %v, want warn (executor applies a default row limit even with no explicit Limit)", got.Level)
	}
	if len(got.Reasons) == 0 {
		t.Error("expected at least one reason")
	}
}

func TestAssessNoTimeRangeWithAggregationRejects(t *testing.T) {
	plan := &ir.Plan{
		TimeRange:   &ir.TimeRange{},
		Aggregation: &ir.Aggregation{Funcs: []ir.AggFunc{{Func: "count", Alias: "count"}}},
	}
	got := Assess(plan)
	if got.Level != LevelReject {
		t.Errorf("Level = %v, want reject for an unbounded aggregation", got.Level)
	}
}

func TestAssessNoTimeRangeWithLimitWarns(t *testing.T) {
	plan := &ir.Plan{
		TimeRange: &ir.TimeRange{},
		Limit:     &ir.Limit{N: 100},
	}
	got := Assess(plan)
	if got.Level != LevelWarn {
		t.Errorf("Level = %v, want warn (limited, no aggregation)", got.Level)
	}
}

func TestAssessBoundedTimeRangeIsOK(t *testing.T) {
	now := time.Now()
	plan := &ir.Plan{
		TimeRange: &ir.TimeRange{From: now.Add(-1 * time.Hour), To: now},
		Limit:     &ir.Limit{N: 100},
	}
	got := Assess(plan)
	if got.Level != LevelOK {
		t.Errorf("Level = %v, want ok, reasons: %v", got.Level, got.Reasons)
	}
}

func TestAssessVeryLargeTimeRangeWarns(t *testing.T) {
	now := time.Now()
	plan := &ir.Plan{
		TimeRange: &ir.TimeRange{From: now.Add(-200 * 24 * time.Hour), To: now},
		Limit:     &ir.Limit{N: 100},
	}
	got := Assess(plan)
	if got.Level != LevelWarn {
		t.Errorf("Level = %v, want warn for a 200-day range", got.Level)
	}
}

func TestAssessRawSQLWithTimestampFilterIsOK(t *testing.T) {
	plan := &ir.Plan{RawSQL: "SELECT count(*) FROM logs WHERE timestamp > now() - INTERVAL 1 HOUR"}
	got := Assess(plan)
	if got.Level != LevelOK {
		t.Errorf("Level = %v, want ok, reasons: %v", got.Level, got.Reasons)
	}
}

func TestAssessRawSQLWithoutTimestampFilterWarns(t *testing.T) {
	plan := &ir.Plan{RawSQL: "SELECT service, count(*) FROM logs GROUP BY service"}
	got := Assess(plan)
	if got.Level != LevelWarn {
		t.Errorf("Level = %v, want warn for raw SQL with no detectable time filter", got.Level)
	}
}

func TestSummaryEmptyForOK(t *testing.T) {
	if s := Summary(Assessment{Level: LevelOK}); s != "" {
		t.Errorf("Summary(OK) = %q, want empty", s)
	}
}

func TestSummaryJoinsReasons(t *testing.T) {
	a := Assessment{Level: LevelWarn, Reasons: []string{"a", "b"}}
	if s := Summary(a); s != "a; b" {
		t.Errorf("Summary = %q, want %q", s, "a; b")
	}
}
