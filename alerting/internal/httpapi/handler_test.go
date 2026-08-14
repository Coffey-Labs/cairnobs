package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sentry/sentry/alerting/internal/notifystore"
	"github.com/sentry/sentry/alerting/internal/rulestore"
)

type fakeRuleStore struct {
	rules map[string]*rulestore.RuleWithState
}

func newFakeRuleStore() *fakeRuleStore {
	return &fakeRuleStore{rules: map[string]*rulestore.RuleWithState{}}
}

func (f *fakeRuleStore) Create(_ context.Context, r *rulestore.Rule) error {
	r.ID = "rule-1"
	f.rules[r.ID] = &rulestore.RuleWithState{Rule: *r, State: rulestore.AlertState{RuleID: r.ID, State: rulestore.StateOK}}
	return nil
}

func (f *fakeRuleStore) List(_ context.Context) ([]rulestore.RuleWithState, error) {
	var out []rulestore.RuleWithState
	for _, r := range f.rules {
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeRuleStore) Get(_ context.Context, id string) (*rulestore.RuleWithState, error) {
	r, ok := f.rules[id]
	if !ok {
		return nil, rulestore.ErrNotFound
	}
	return r, nil
}

func (f *fakeRuleStore) Delete(_ context.Context, id string) error {
	if _, ok := f.rules[id]; !ok {
		return rulestore.ErrNotFound
	}
	delete(f.rules, id)
	return nil
}

type fakeTargetStore struct {
	targets map[string]*notifystore.Target
}

func newFakeTargetStore() *fakeTargetStore {
	return &fakeTargetStore{targets: map[string]*notifystore.Target{}}
}

func (f *fakeTargetStore) Create(_ context.Context, t *notifystore.Target) error {
	t.ID = "target-1"
	f.targets[t.ID] = t
	return nil
}
func (f *fakeTargetStore) List(_ context.Context) ([]notifystore.Target, error) {
	var out []notifystore.Target
	for _, t := range f.targets {
		out = append(out, *t)
	}
	return out, nil
}
func (f *fakeTargetStore) Get(_ context.Context, id string) (*notifystore.Target, error) {
	t, ok := f.targets[id]
	if !ok {
		return nil, notifystore.ErrNotFound
	}
	return t, nil
}
func (f *fakeTargetStore) Delete(_ context.Context, id string) error {
	if _, ok := f.targets[id]; !ok {
		return notifystore.ErrNotFound
	}
	delete(f.targets, id)
	return nil
}

type fakeDeliveryReader struct {
	entries []rulestore.DeliveryLogEntry
}

func (f *fakeDeliveryReader) ListForRule(_ context.Context, _ string, _ int) ([]rulestore.DeliveryLogEntry, error) {
	return f.entries, nil
}

func newTestMux(rules ruleStore, targets targetStore, deliveries deliveryReader) *http.ServeMux {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), rules, targets, deliveries)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateThresholdRule(t *testing.T) {
	targets := newFakeTargetStore()
	targets.targets["target-1"] = &notifystore.Target{ID: "target-1"}
	mux := newTestMux(newFakeRuleStore(), targets, &fakeDeliveryReader{})

	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "High error rate", "query": "service=api | where status>=500 | stats count",
		"condition_type": "threshold", "comparator": "gt", "threshold_value": 100,
		"eval_interval_seconds": 60, "notification_target_id": "target-1"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateRuleDefaultsToEnabledWhenOmitted guards against a real bug
// caught by actually calling this endpoint: a plain `bool` JSON field
// can't distinguish "omitted" from "explicitly false," and Go's zero
// value for bool is false -- without createRuleRequest's *bool handling,
// a create request that simply didn't mention "enabled" silently created
// a rule the evaluator's claim query would never pick up.
func TestCreateRuleDefaultsToEnabledWhenOmitted(t *testing.T) {
	rules := newFakeRuleStore()
	mux := newTestMux(rules, newFakeTargetStore(), &fakeDeliveryReader{})

	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "no enabled field", "query": "service=api", "condition_type": "absence",
		"eval_interval_seconds": 60, "notification_target_id": "target-1"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !rules.rules["rule-1"].Enabled {
		t.Fatalf("expected a rule created without an explicit \"enabled\" field to default to enabled=true")
	}
}

func TestCreateRuleRespectsExplicitDisabled(t *testing.T) {
	rules := newFakeRuleStore()
	mux := newTestMux(rules, newFakeTargetStore(), &fakeDeliveryReader{})

	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "explicitly disabled", "query": "service=api", "condition_type": "absence",
		"eval_interval_seconds": 60, "notification_target_id": "target-1", "enabled": false
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if rules.rules["rule-1"].Enabled {
		t.Fatalf("expected an explicit \"enabled\": false to be respected")
	}
}

func TestCreateThresholdRuleRejectsMissingComparator(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "bad rule", "query": "service=api", "condition_type": "threshold",
		"eval_interval_seconds": 60, "notification_target_id": "target-1"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAbsenceRuleDoesNotRequireComparator(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "no heartbeat", "query": "service=payments earliest=-5m", "condition_type": "absence",
		"eval_interval_seconds": 60, "notification_target_id": "target-1"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateRuleRejectsShortInterval(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodPost, "/rules", `{
		"name": "too fast", "query": "service=api", "condition_type": "absence",
		"eval_interval_seconds": 5, "notification_target_id": "target-1"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetRuleNotFound(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodGet, "/rules/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteRule(t *testing.T) {
	rules := newFakeRuleStore()
	rules.rules["rule-1"] = &rulestore.RuleWithState{Rule: rulestore.Rule{ID: "rule-1"}}
	mux := newTestMux(rules, newFakeTargetStore(), &fakeDeliveryReader{})

	rec := doRequest(t, mux, http.MethodDelete, "/rules/rule-1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if _, ok := rules.rules["rule-1"]; ok {
		t.Fatalf("expected rule to be deleted")
	}
}

func TestCreateTargetRejectsInvalidKind(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodPost, "/targets", `{"name": "x", "kind": "carrier-pigeon", "webhook_url": "https://example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateSlackTarget(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodPost, "/targets", `{"name": "oncall", "kind": "slack", "webhook_url": "https://hooks.slack.com/services/x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListDeliveriesForRule(t *testing.T) {
	deliveries := &fakeDeliveryReader{entries: []rulestore.DeliveryLogEntry{
		{ID: 1, RuleID: "rule-1", EventType: "firing", Status: "sent"},
	}}
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), deliveries)

	rec := doRequest(t, mux, http.MethodGet, "/rules/rule-1/deliveries", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"sent"`) {
		t.Fatalf("expected delivery entry in response, got: %s", rec.Body.String())
	}
}

func TestHandleHealthz(t *testing.T) {
	mux := newTestMux(newFakeRuleStore(), newFakeTargetStore(), &fakeDeliveryReader{})
	rec := doRequest(t, mux, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
