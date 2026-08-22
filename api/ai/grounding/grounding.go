// Package grounding builds provider.SchemaContext from a tenant's own
// ClickHouse data -- known service names, common attribute keys, and
// example values for enum-like fields -- sourced by periodic sampling,
// never hand-maintained (task 3). See /docs/phase-7-ai-design.md's
// "Schema grounding" section for the embedded-in-prompt-vs-retrieved
// tradeoff this package's shape is built around.
//
// Tenant scoping is structural, not a filter this package applies: a
// Service wraps exactly one executor.SQLRunner, and that SQLRunner is
// already tenant-scoped by whoever constructed it (the plain shared
// runner in a single-tenant deployment, or one specific tenant's
// chrunner-resolved connection in enterprise-api) -- the same connection-
// layer isolation discipline Phase 4 established for query execution
// applies here for free, because grounding queries run through the exact
// same SQLRunner interface, never a separate admin/shared connection.
// A multi-tenant deployment needs one Service per active tenant --
// enterprise/internal/groundingregistry provides that, mirroring
// enterprise/internal/chwriter.Registry's per-tenant-instance shape.
package grounding

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/api/ai/provider"
	"github.com/cairnobs/cairnobs/api/querylang/executor"
)

// staticFields are always present regardless of what's actually been
// ingested -- Phase 0's schema (storage/migrations/0001_create_logs_table.sql)
// plus record_id (0002). Listed here rather than queried: their existence
// doesn't depend on sampling, only their *values* do (severity's examples
// still come from a real query below, in case a deployment's severities
// diverge from the standard OTel set).
var staticFields = []string{"timestamp", "host", "service", "severity", "message", "record_id"}

// Tuning constants. First-pass values, not benchmarked against a
// production-scale cluster -- see the "not yet verified at scale" note
// in /docs/phase-7-ai-design.md. Deliberately conservative (short lookback,
// small caps) since grounding data trades completeness for prompt-budget
// and refresh-query cost, not the other way around.
const (
	sampleWindow         = 7 * 24 * time.Hour // how far back sampling queries look
	maxServices          = 50
	maxAttributeKeys     = 100                     // how many keys we learn about at all
	maxEnumCandidateKeys = 15                      // of those, how many get a real example-value query (each is a separate round trip)
	maxExamplesPerField  = 20                      // a field returning more distinct values than this in the capped query isn't treated as enum-like
	perFieldQueryLimit   = maxExamplesPerField + 1 // +1 so "more than maxExamplesPerField" is detectable, not just silently truncated
)

// Service produces provider.SchemaContext for one tenant (or, in a
// single-tenant deployment, the whole instance) from its own ClickHouse
// data. Safe for concurrent use: Refresh swaps a snapshot under a mutex,
// Current reads it under the same lock -- same last-known-good pattern
// enterprise/internal/chwriter.Registry and search/src/tenants.rs's
// ActiveTenantTracker already use, so a slow or failing refresh never
// blocks or blanks a caller mid-request.
type Service struct {
	runner executor.SQLRunner

	mu       sync.RWMutex
	snapshot provider.SchemaContext
}

func New(runner executor.SQLRunner) *Service {
	return &Service{runner: runner}
}

// Current returns the last successfully refreshed SchemaContext --
// possibly stale, possibly zero-valued if Refresh has never succeeded
// yet, but never a partial/torn snapshot. Callers (the AI operation
// handlers, task 5+) should treat a zero-valued Services/Fields as "no
// grounding data yet available," not an error -- every operation still
// works with an empty SchemaContext, just less well-grounded, matching
// this codebase's "absence is a normal state, not a failure" convention
// (e.g. AuditLogger, getAuthFeatures).
func (s *Service) Current() provider.SchemaContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// SchemaContext implements aiapi.SchemaContextSource directly -- a
// single-tenant deployment has exactly one Service, so there's no
// per-request tenant resolution to do here (ctx is unused); it's the
// same shape as Current, just satisfying the interface aiapi's handlers
// depend on so main.go can wire *Service in without a separate adapter
// type. enterprise-api's multi-tenant equivalent (groundingregistry)
// implements this same interface by actually reading ctx.
func (s *Service) SchemaContext(context.Context) provider.SchemaContext {
	return s.Current()
}

// Refresh runs the sampling queries and swaps the cached snapshot on
// success. A failed refresh leaves the previous snapshot in place
// (last-known-good) rather than clearing it -- a transient ClickHouse
// hiccup shouldn't blank out grounding for every AI request until the
// next successful refresh.
func (s *Service) Refresh(ctx context.Context) error {
	services, err := s.sampleServices(ctx)
	if err != nil {
		return fmt.Errorf("grounding: sampling services: %w", err)
	}

	attrKeys, err := s.sampleAttributeKeys(ctx)
	if err != nil {
		return fmt.Errorf("grounding: sampling attribute keys: %w", err)
	}

	fields := make([]provider.FieldInfo, 0, len(staticFields)+len(attrKeys))
	for _, name := range staticFields {
		examples, _ := s.sampleFieldExamples(ctx, name, name == "severity")
		fields = append(fields, provider.FieldInfo{Name: name, Examples: examples})
	}

	candidateKeys := attrKeys
	if len(candidateKeys) > maxEnumCandidateKeys {
		candidateKeys = candidateKeys[:maxEnumCandidateKeys]
	}
	enumExamples := make(map[string][]string, len(candidateKeys))
	for _, key := range candidateKeys {
		examples, ok := s.sampleFieldExamples(ctx, key, false)
		if ok {
			enumExamples[key] = examples
		}
	}
	for _, key := range attrKeys {
		fields = append(fields, provider.FieldInfo{Name: key, Examples: enumExamples[key]})
	}

	s.mu.Lock()
	s.snapshot = provider.SchemaContext{Services: services, Fields: fields}
	s.mu.Unlock()
	return nil
}

// StartRefreshing runs Refresh once immediately (best-effort -- a failure
// here just means Current() returns a zero snapshot until the first
// successful tick, not a fatal startup error, since grounding is an
// enhancement, not a dependency anything else blocks on) and then on
// interval until ctx is cancelled. Same shape as chwriter.Registry.
// StartRefreshing.
func (s *Service) StartRefreshing(ctx context.Context, interval time.Duration, onError func(error)) {
	if err := s.Refresh(ctx); err != nil && onError != nil {
		onError(err)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Refresh(ctx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}

func (s *Service) sampleServices(ctx context.Context) ([]string, error) {
	sql := fmt.Sprintf(
		"SELECT service FROM logs WHERE timestamp > now() - INTERVAL %d SECOND GROUP BY service ORDER BY count() DESC LIMIT %d",
		int(sampleWindow.Seconds()), maxServices,
	)
	res, err := s.runner.RunSQL(ctx, sql)
	if err != nil {
		return nil, err
	}
	return firstColumnStrings(res), nil
}

func (s *Service) sampleAttributeKeys(ctx context.Context) ([]string, error) {
	sql := fmt.Sprintf(
		"SELECT arrayJoin(mapKeys(attributes)) AS attr_key FROM logs WHERE timestamp > now() - INTERVAL %d SECOND GROUP BY attr_key ORDER BY count() DESC LIMIT %d",
		int(sampleWindow.Seconds()), maxAttributeKeys,
	)
	res, err := s.runner.RunSQL(ctx, sql)
	if err != nil {
		return nil, err
	}
	return firstColumnStrings(res), nil
}

// sampleFieldExamples returns up to maxExamplesPerField distinct values
// for a field, and false if the field turned out not to look enum-like
// (more distinct values than the cap turned up, or the value column
// wasn't usable) -- matching FieldInfo's doc comment that a
// high-cardinality field should carry no examples rather than a
// truncated, misleading sample. isStructuredColumn distinguishes
// `severity` (a real column) from an attributes[...] lookup.
func (s *Service) sampleFieldExamples(ctx context.Context, field string, isStructuredColumn bool) ([]string, bool) {
	col := "attributes[" + quoteLiteral(field) + "]"
	if isStructuredColumn {
		col = "`" + field + "`"
	}
	sql := fmt.Sprintf(
		"SELECT DISTINCT %s AS v FROM logs WHERE timestamp > now() - INTERVAL %d SECOND AND %s != '' LIMIT %d",
		col, int(sampleWindow.Seconds()), col, perFieldQueryLimit,
	)
	res, err := s.runner.RunSQL(ctx, sql)
	if err != nil {
		return nil, false
	}
	values := firstColumnStrings(res)
	if len(values) == 0 || len(values) > maxExamplesPerField {
		return nil, false
	}
	return values, true
}

func firstColumnStrings(res *executor.Result) []string {
	if res == nil || len(res.Columns) == 0 {
		return nil
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) == 0 {
			continue
		}
		if s, ok := row[0].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// quoteLiteral must stay byte-for-byte in sync with executor/sql.go's
// unexported function of the same name (ClickHouse SQL string literals
// use backslash escaping, not SQL-standard doubled quotes -- easy to get
// wrong by assuming the more common convention, which an earlier draft
// of this function did). Duplicated rather than exported from executor,
// since executor's quoteLiteral is deliberately unexported (query-SQL
// building is that package's own concern) and grounding's use is
// narrow enough not to justify widening that package's public surface
// for one helper.
func quoteLiteral(s string) string {
	var sb strings.Builder
	sb.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '\'':
			sb.WriteString(`\'`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('\'')
	return sb.String()
}
