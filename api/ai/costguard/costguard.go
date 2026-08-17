// Package costguard is the shared cost/safety check task 4 asked for --
// no such mechanism existed anywhere in Phase 2/3's compiler before this
// (confirmed by reading planner.go/sql.go before writing this: plan.TimeRange
// can be entirely unset, and nothing downstream rejects that). Built here
// as a standalone, pure function operating on the same ir.Plan every
// query -- hand-written or AI-generated -- already compiles to, so there
// is exactly one cost check, not one per code path.
//
// This package does not decide what a caller *does* with a Reject-level
// Assessment -- see /docs/phase-7-ai-design.md's "Cost/safety guard"
// section for how the AI tracks and the existing /query handler each
// apply this differently (AI suggestions withhold a Reject-level
// suggestion from being offered as directly runnable; the existing
// /query handler surfaces the same assessment as a non-blocking warning,
// deliberately not a new hard block on hand-written queries this phase
// didn't set out to change).
package costguard

import (
	"regexp"
	"strings"
	"time"

	"github.com/sentry/sentry/api/internal/querylang/ir"
)

type Level string

const (
	LevelOK     Level = "ok"
	LevelWarn   Level = "warn"
	LevelReject Level = "reject"
)

type Assessment struct {
	Level   Level
	Reasons []string
}

// maxReasonableSpan and the two below are first-pass heuristic
// thresholds, not benchmarked against a production-scale ClickHouse
// cluster -- this environment's own data is far smaller than what these
// numbers are meant to guard against. Flagged explicitly in
// /docs/phase-7-ai-design.md rather than presented as tuned. Revisit
// once there's real cluster-size data to check them against.
const maxReasonableSpan = 90 * 24 * time.Hour

// rawSQLTimestampRe is a best-effort, deliberately loose check for
// *some* mention of the timestamp column in a raw SQL statement's WHERE
// clause -- not a real SQL parser. A false negative here (a query that
// does filter by time in a way this regex doesn't recognize) just means
// an unnecessary Warn, not a Reject, so being loose-but-safe is the
// right failure direction. Raw SQL genuinely can't get the same
// structural guarantee the IR-based checks below get, and this package
// says so rather than pretending otherwise.
var rawSQLTimestampRe = regexp.MustCompile(`(?i)\btimestamp\b\s*[<>=]`)

// Assess evaluates one compiled plan. Never returns an error -- a plan
// that reached this point already parsed successfully; this is a
// judgment call about cost, not a correctness check.
func Assess(plan *ir.Plan) Assessment {
	if plan.RawSQL != "" {
		return assessRawSQL(plan.RawSQL)
	}
	return assessIR(plan)
}

func assessRawSQL(sql string) Assessment {
	if rawSQLTimestampRe.MatchString(sql) {
		return Assessment{Level: LevelOK}
	}
	return Assessment{
		Level: LevelWarn,
		Reasons: []string{
			"no obvious timestamp filter found in this raw SQL -- this is a best-effort text check, not a real parse, so it may be wrong in either direction, but if this query has no time bound it could scan the full table's history",
		},
	}
}

func assessIR(plan *ir.Plan) Assessment {
	var reasons []string
	level := LevelOK

	hasTimeBound := plan.TimeRange != nil && (!plan.TimeRange.From.IsZero() || !plan.TimeRange.To.IsZero())

	if !hasTimeBound {
		switch {
		case plan.Aggregation != nil:
			// Unlike a raw-row query, an aggregation gets no implicit
			// row cap from the executor regardless of plan.Limit --
			// see executor/sql.go's buildSQL: the defaultRowLimit
			// safety net only applies `else if plan.Aggregation ==
			// nil`. An unbounded aggregation is never merely
			// "capped but slow" the way a raw-row fetch is.
			level = LevelReject
			reasons = append(reasons, "no time range filter, and this query aggregates -- every matching row across the table's entire history must be scanned to compute the aggregate, regardless of how small the output is")
		default:
			// A raw-row query with no explicit Limit still gets
			// executor/sql.go's defaultRowLimit=100 safety net applied
			// automatically -- it is not actually unbounded output,
			// just potentially an expensive scan to find those rows
			// without a time bound to narrow the search. Confirmed by
			// reading buildSQL directly, not assumed: this is the same
			// risk level whether plan.Limit is nil or explicitly set,
			// so both cases share one Warn, not a Reject for one and a
			// Warn for the other.
			level = LevelWarn
			reasons = append(reasons, "no time range filter -- results are capped (explicitly, or by the default 100-row limit), but ClickHouse may still need to scan well beyond that many rows to find them without a time bound to narrow the search")
		}
		if len(plan.TextSearch) > 0 {
			reasons = append(reasons, "the free-text search stage is bounded by the existing 5,000-record Tantivy prefilter cap regardless of time range, which partially limits how bad this is, but doesn't remove the underlying ClickHouse-side cost")
		}
	} else if !plan.TimeRange.From.IsZero() && !plan.TimeRange.To.IsZero() {
		span := plan.TimeRange.To.Sub(plan.TimeRange.From)
		if span > maxReasonableSpan {
			level = maxLevel(level, LevelWarn)
			reasons = append(reasons, "time range spans more than 90 days -- this may be slow depending on data volume")
		}
	}

	return Assessment{Level: level, Reasons: reasons}
}

func maxLevel(a, b Level) Level {
	rank := map[Level]int{LevelOK: 0, LevelWarn: 1, LevelReject: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Summary renders an Assessment as one human-readable line, for
// embedding in an AI-suggestion response or a /query warnings entry --
// one shared rendering so the two callers don't independently invent
// slightly different phrasing for the same underlying reasons.
func Summary(a Assessment) string {
	if a.Level == LevelOK || len(a.Reasons) == 0 {
		return ""
	}
	return strings.Join(a.Reasons, "; ")
}
