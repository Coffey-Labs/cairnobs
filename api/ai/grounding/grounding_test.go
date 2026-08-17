package grounding

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sentry/sentry/api/ai/provider"
	"github.com/sentry/sentry/api/querylang/executor"
)

// routingFakeRunner returns a canned result keyed by a substring match
// against the SQL text -- grounding.Refresh issues several structurally
// different queries in sequence (services, attribute keys, then one
// per candidate enum field), unlike executor's tests where a single
// fixed result/err per call is enough.
type routingFakeRunner struct {
	byContains []struct {
		substr string
		result *executor.Result
		err    error
	}
	calls int
}

func (r *routingFakeRunner) on(substr string, result *executor.Result) *routingFakeRunner {
	r.byContains = append(r.byContains, struct {
		substr string
		result *executor.Result
		err    error
	}{substr, result, nil})
	return r
}

func (r *routingFakeRunner) RunSQL(_ context.Context, sql string) (*executor.Result, error) {
	r.calls++
	for _, rule := range r.byContains {
		if strings.Contains(sql, rule.substr) {
			if rule.err != nil {
				return nil, rule.err
			}
			return rule.result, nil
		}
	}
	// Unmatched queries (most of the per-field example queries in a
	// small test fixture) come back empty, same as a field with no data
	// -- not an error, matching sampleFieldExamples' "unusable -> not
	// enum-like" treatment.
	return &executor.Result{Columns: []string{"v"}, Rows: nil}, nil
}

func strResult(col string, vals ...string) *executor.Result {
	rows := make([][]any, len(vals))
	for i, v := range vals {
		rows[i] = []any{v}
	}
	return &executor.Result{Columns: []string{col}, Rows: rows}
}

func TestRefreshPopulatesServicesAndFields(t *testing.T) {
	runner := (&routingFakeRunner{}).
		on("FROM logs WHERE timestamp > now() - INTERVAL 604800 SECOND GROUP BY service", strResult("service", "api", "web", "worker")).
		on("mapKeys(attributes)", strResult("attr_key", "status", "latency_ms")).
		on("`severity`", strResult("v", "INFO", "WARN", "ERROR")).
		on("attributes['status']", strResult("v", "200", "404", "500"))

	svc := New(runner)
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	got := svc.Current()
	if len(got.Services) != 3 || got.Services[0] != "api" {
		t.Errorf("Services = %v, want [api web worker]", got.Services)
	}

	var severity, status *provider.FieldInfo
	for i := range got.Fields {
		f := &got.Fields[i]
		switch f.Name {
		case "severity":
			severity = f
		case "status":
			status = f
		}
	}
	if severity == nil || len(severity.Examples) != 3 {
		t.Errorf("severity field = %+v, want 3 examples", severity)
	}
	if status == nil || len(status.Examples) != 3 {
		t.Errorf("status field = %+v, want 3 examples", status)
	}

	// Static fields with no configured example rule (host, message,
	// timestamp, record_id) should still be present, just with no
	// examples -- Refresh must not drop them.
	names := make(map[string]bool, len(got.Fields))
	for _, f := range got.Fields {
		names[f.Name] = true
	}
	for _, want := range []string{"timestamp", "host", "message", "record_id"} {
		if !names[want] {
			t.Errorf("static field %q missing from Fields", want)
		}
	}
}

func TestRefreshFailureLeavesPreviousSnapshot(t *testing.T) {
	good := (&routingFakeRunner{}).
		on("GROUP BY service", strResult("service", "api"))
	svc := New(good)
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	first := svc.Current()

	svc.runner = &erroringRunner{}
	if err := svc.Refresh(context.Background()); err == nil {
		t.Fatal("expected Refresh to fail with an erroring runner")
	}

	after := svc.Current()
	if len(after.Services) != len(first.Services) || after.Services[0] != first.Services[0] {
		t.Errorf("Current() after a failed Refresh = %+v, want unchanged snapshot %+v", after, first)
	}
}

type erroringRunner struct{}

func (erroringRunner) RunSQL(context.Context, string) (*executor.Result, error) {
	return nil, context.DeadlineExceeded
}

func TestFieldExampleCapExcludesHighCardinalityFields(t *testing.T) {
	many := make([]string, maxExamplesPerField+1)
	for i := range many {
		many[i] = string(rune('a' + i%26))
	}
	runner := (&routingFakeRunner{}).
		on("GROUP BY service", strResult("service", "api")).
		on("mapKeys(attributes)", strResult("attr_key", "trace_id")).
		on("attributes['trace_id']", strResult("v", many...))

	svc := New(runner)
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	for _, f := range svc.Current().Fields {
		if f.Name == "trace_id" && len(f.Examples) != 0 {
			t.Errorf("high-cardinality field trace_id got %d examples, want 0 (not treated as enum-like)", len(f.Examples))
		}
	}
}

func TestStartRefreshingRunsOnInterval(t *testing.T) {
	runner := (&routingFakeRunner{}).on("GROUP BY service", strResult("service", "api"))
	svc := New(runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartRefreshing(ctx, 10*time.Millisecond, nil)

	// The immediate synchronous refresh should have already happened by
	// the time StartRefreshing returns.
	if got := svc.Current().Services; len(got) != 1 {
		t.Fatalf("Current() immediately after StartRefreshing = %v, want [api]", got)
	}
}
