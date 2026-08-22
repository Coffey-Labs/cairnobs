package logretention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sentry/sentry/api/authz"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// targetCall records one CountOlderThan/DeleteOlderThan invocation, so
// tests can assert both the cutoff and the exact target set a call used.
type targetCall struct {
	cutoff  time.Time
	targets []HostService
}

// fakeStore lets a test inject store errors and a fixed target listing,
// and records every count/delete call it received so tests can assert
// the handler scoped them to the right targets.
type fakeStore struct {
	targetList  []TargetCount
	targetsErr  error
	count       uint64
	countErr    error
	deleteErr   error
	countedWith []targetCall
	deletedWith []targetCall
}

func (f *fakeStore) TargetsOlderThan(_ context.Context, _ time.Time) ([]TargetCount, error) {
	if f.targetsErr != nil {
		return nil, f.targetsErr
	}
	return f.targetList, nil
}

func (f *fakeStore) CountOlderThan(_ context.Context, cutoff time.Time, targets []HostService) (uint64, error) {
	f.countedWith = append(f.countedWith, targetCall{cutoff, targets})
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func (f *fakeStore) DeleteOlderThan(_ context.Context, cutoff time.Time, targets []HostService) error {
	f.deletedWith = append(f.deletedWith, targetCall{cutoff, targets})
	return f.deleteErr
}

type fakeAuthorizer struct {
	role authz.Role
}

func (f fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return authz.Identity{TenantID: "default", UserID: "u1", Role: f.role}, nil
}

// fakeFloor stands in for AgentRetentionStore -- a nil/empty byHost map
// means no agent has any retention floor configured, same as every
// test that doesn't care about the floor assumed before it existed.
type fakeFloor struct {
	byHost map[string]HostFloor
	err    error
}

func (f fakeFloor) FloorsByHost(context.Context) (map[string]HostFloor, error) {
	return f.byHost, f.err
}

func newTestHandler(s *fakeStore, role authz.Role) *Handler {
	return NewHandler(discardLogger(), s, fakeFloor{}, fakeAuthorizer{role: role})
}

func doRequest(t *testing.T, h *Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func doJSONRequest(t *testing.T, h *Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling request body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func hoursForDays(days int) int {
	return days * 24
}

func intPtr(n int) *int { return &n }

func TestHostsListsTargetsGroupedByHostWithFloors(t *testing.T) {
	s := &fakeStore{targetList: []TargetCount{
		{Host: "web-01", Service: "nginx", Count: 100},
		{Host: "web-01", Service: "smtp", Count: 5},
		{Host: "web-02", Service: "ufw", Count: 20},
	}}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {DefaultDays: intPtr(7), ServiceDays: map[string]int{"smtp": 365}},
	}}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=24")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp hostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(resp.Hosts))
	}

	web01 := resp.Hosts[0]
	if web01.Host != "web-01" || web01.ProtectedDays == nil || *web01.ProtectedDays != 7 {
		t.Fatalf("hosts[0] = %+v, want web-01 with host default floor 7", web01)
	}
	if len(web01.Services) != 2 {
		t.Fatalf("web-01 services = %+v, want 2 entries", web01.Services)
	}
	if web01.Services[0].Service != "nginx" || web01.Services[0].Count != 100 || web01.Services[0].ProtectedDays == nil || *web01.Services[0].ProtectedDays != 7 {
		t.Errorf("web-01/nginx = %+v, want count=100 protected_days=7 (host default)", web01.Services[0])
	}
	if web01.Services[1].Service != "smtp" || web01.Services[1].ProtectedDays == nil || *web01.Services[1].ProtectedDays != 365 {
		t.Errorf("web-01/smtp = %+v, want protected_days=365 (service override, not the host default)", web01.Services[1])
	}

	web02 := resp.Hosts[1]
	if web02.Host != "web-02" || web02.ProtectedDays != nil {
		t.Fatalf("hosts[1] = %+v, want web-02 with no floor", web02)
	}
	if len(web02.Services) != 1 || web02.Services[0].ProtectedDays != nil {
		t.Errorf("web-02 services = %+v, want ufw with no floor", web02.Services)
	}
}

func TestPreviewReturnsCountCutoffAndTargets(t *testing.T) {
	s := &fakeStore{count: 42}
	h := newTestHandler(s, authz.RoleAdmin)

	before := time.Now().UTC()
	rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{
		OlderThanHours: 24,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}, {Host: "web-01", Service: "smtp"}},
	})
	after := time.Now().UTC()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp previewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Count != 42 {
		t.Errorf("count = %d, want 42", resp.Count)
	}
	want := []HostService{{Host: "web-01", Service: "nginx"}, {Host: "web-01", Service: "smtp"}}
	if !reflect.DeepEqual(resp.Targets, want) {
		t.Errorf("targets = %v, want %v", resp.Targets, want)
	}
	wantEarliest := before.Add(-24 * time.Hour)
	wantLatest := after.Add(-24 * time.Hour)
	if resp.Cutoff.Before(wantEarliest) || resp.Cutoff.After(wantLatest) {
		t.Errorf("cutoff = %v, want between %v and %v", resp.Cutoff, wantEarliest, wantLatest)
	}
	if len(s.deletedWith) != 0 {
		t.Errorf("preview must never delete anything, but DeleteOlderThan was called %d time(s)", len(s.deletedWith))
	}
	if len(s.countedWith) != 1 || !reflect.DeepEqual(s.countedWith[0].targets, want) {
		t.Errorf("CountOlderThan was not scoped to the requested targets: %+v", s.countedWith)
	}
}

func TestPreviewDedupesTargets(t *testing.T) {
	s := &fakeStore{count: 1}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{
		OlderThanHours: 24,
		Targets: []HostService{
			{Host: "web-01", Service: "nginx"},
			{Host: "web-01", Service: "nginx"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	want := []HostService{{Host: "web-01", Service: "nginx"}}
	if len(s.countedWith) != 1 || !reflect.DeepEqual(s.countedWith[0].targets, want) {
		t.Fatalf("expected a deduped single-target call, got %+v", s.countedWith)
	}
}

func TestPreviewRequiresAtLeastOneTarget(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{OlderThanHours: 24})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no targets specified", rec.Code)
	}
}

func TestPreviewRejectsTargetWithEmptyHostOrService(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	cases := [][]HostService{
		{{Host: "", Service: "nginx"}},
		{{Host: "web-01", Service: ""}},
	}
	for _, targets := range cases {
		rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{OlderThanHours: 24, Targets: targets})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("targets %v: status = %d, want 400", targets, rec.Code)
		}
	}
}

func TestPreviewRejectsInvalidOlderThanHours(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)
	targets := []HostService{{Host: "web-01", Service: "nginx"}}

	cases := []int{0, -5, 999999999}
	for _, hours := range cases {
		rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{OlderThanHours: hours, Targets: targets})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("older_than_hours=%d: status = %d, want 400", hours, rec.Code)
		}
	}
}

func TestDeleteReturnsDeletedCountTargetsAndCutoff(t *testing.T) {
	s := &fakeStore{count: 7}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: 720,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.DeletedCount != 7 {
		t.Errorf("deleted_count = %d, want 7", resp.DeletedCount)
	}
	want := []HostService{{Host: "web-01", Service: "nginx"}}
	if !reflect.DeepEqual(resp.DeletedTargets, want) {
		t.Errorf("deleted_targets = %v, want %v", resp.DeletedTargets, want)
	}
	if len(s.deletedWith) != 1 || !reflect.DeepEqual(s.deletedWith[0].targets, want) {
		t.Fatalf("expected exactly one scoped DeleteOlderThan call, got %+v", s.deletedWith)
	}
	if len(s.countedWith) != 1 || !s.countedWith[0].cutoff.Equal(s.deletedWith[0].cutoff) {
		t.Errorf("count and delete must use the same cutoff: counted=%+v deleted=%+v", s.countedWith, s.deletedWith)
	}
}

func TestDeleteRejectsMissingTargets(t *testing.T) {
	s := &fakeStore{}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{OlderThanHours: 24})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no targets specified", rec.Code)
	}
	if len(s.deletedWith) != 0 {
		t.Error("a request with no targets specified must never reach the store's delete path")
	}
}

func TestDeleteRejectsInvalidJSONBody(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	req := httptest.NewRequest("POST", "/logs/retention/delete", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeletePropagatesStoreErrors(t *testing.T) {
	s := &fakeStore{deleteErr: errors.New("clickhouse mutation failed")}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: 24,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestOwnerAndAdminCanUseRetentionRoutes(t *testing.T) {
	targets := []HostService{{Host: "web-01", Service: "nginx"}}
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleOwner} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		hosts := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=24")
		if hosts.Code != http.StatusOK {
			t.Errorf("role %s: hosts status = %d, want 200", role, hosts.Code)
		}
		preview := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{OlderThanHours: 24, Targets: targets})
		if preview.Code != http.StatusOK {
			t.Errorf("role %s: preview status = %d, want 200", role, preview.Code)
		}
		del := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{OlderThanHours: 24, Targets: targets})
		if del.Code != http.StatusOK {
			t.Errorf("role %s: delete status = %d, want 200", role, del.Code)
		}
	}
}

func TestViewerAndEditorAreForbiddenFromRetentionRoutes(t *testing.T) {
	targets := []HostService{{Host: "web-01", Service: "nginx"}}
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleEditor} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		hosts := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=24")
		if hosts.Code != http.StatusForbidden {
			t.Errorf("role %s: hosts status = %d, want 403", role, hosts.Code)
		}
		preview := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{OlderThanHours: 24, Targets: targets})
		if preview.Code != http.StatusForbidden {
			t.Errorf("role %s: preview status = %d, want 403", role, preview.Code)
		}
		del := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{OlderThanHours: 24, Targets: targets})
		if del.Code != http.StatusForbidden {
			t.Errorf("role %s: delete status = %d, want 403", role, del.Code)
		}
		if len(s.deletedWith) != 0 {
			t.Errorf("role %s: must never reach the store", role)
		}
	}
}

func TestRetentionRoutesRequireAuth(t *testing.T) {
	s := &fakeStore{}
	h := NewHandler(discardLogger(), s, fakeFloor{}, nil)

	// A nil authorizer is Phase 0-3's default-open behavior (see
	// authz.RequireRole's doc comment) -- confirm that posture applies
	// here too, same as every other RequireRole-wrapped route, rather
	// than this package accidentally being open or closed by default in
	// a way inconsistent with the rest of the API.
	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: 24,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status with nil authorizer = %d, want 200 (default-open, matches RequireRole elsewhere)", rec.Code)
	}
}

// TestAdminPartiallyBlockedByPerTargetRetentionFloor is the core
// regression test for target-scoped floor enforcement: requesting two
// targets where only one has a protective floor must delete the
// unprotected target and report the other as blocked, not reject the
// whole request.
func TestAdminPartiallyBlockedByPerTargetRetentionFloor(t *testing.T) {
	s := &fakeStore{count: 5}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {ServiceDays: map[string]int{"smtp": 90}},
	}}, fakeAuthorizer{role: authz.RoleAdmin})

	// 30 days is newer than smtp's 90-day floor.
	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: hoursForDays(30),
		Targets:        []HostService{{Host: "web-01", Service: "smtp"}, {Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial success, not an error), body=%s", rec.Code, rec.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	wantDeleted := []HostService{{Host: "web-01", Service: "nginx"}}
	if !reflect.DeepEqual(resp.DeletedTargets, wantDeleted) {
		t.Errorf("deleted_targets = %v, want %v", resp.DeletedTargets, wantDeleted)
	}
	if len(resp.BlockedTargets) != 1 || resp.BlockedTargets[0] != (blockedTarget{Host: "web-01", Service: "smtp", ProtectedDays: 90}) {
		t.Errorf("blocked_targets = %+v, want [{web-01 smtp 90}]", resp.BlockedTargets)
	}
	if len(s.deletedWith) != 1 || !reflect.DeepEqual(s.deletedWith[0].targets, wantDeleted) {
		t.Fatalf("DeleteOlderThan must only ever be scoped to the allowed target, got %+v", s.deletedWith)
	}
}

// TestServiceOverrideBeatsHostDefault confirms Effective's precedence:
// a service-specific floor applies over the host default even when the
// host default alone would have allowed the request.
func TestServiceOverrideBeatsHostDefault(t *testing.T) {
	s := &fakeStore{count: 5}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {DefaultDays: intPtr(7), ServiceDays: map[string]int{"smtp": 365}},
	}}, fakeAuthorizer{role: authz.RoleAdmin})

	// 30 days clears the 7-day host default but not smtp's 365-day
	// override.
	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: hoursForDays(30),
		Targets:        []HostService{{Host: "web-01", Service: "smtp"}, {Host: "web-01", Service: "nginx"}},
	})
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	wantDeleted := []HostService{{Host: "web-01", Service: "nginx"}}
	if !reflect.DeepEqual(resp.DeletedTargets, wantDeleted) {
		t.Errorf("deleted_targets = %v, want %v (nginx uses the 7-day default, smtp its own 365-day override)", resp.DeletedTargets, wantDeleted)
	}
	if len(resp.BlockedTargets) != 1 || resp.BlockedTargets[0].ProtectedDays != 365 {
		t.Errorf("blocked_targets = %+v, want smtp blocked at 365 days", resp.BlockedTargets)
	}
}

// TestAllTargetsBlockedReturnsZeroCountNotError confirms a request
// where every requested target is protected still succeeds (200), just
// with nothing deleted -- informative, not an error condition, since
// the request itself was perfectly valid.
func TestAllTargetsBlockedReturnsZeroCountNotError(t *testing.T) {
	s := &fakeStore{count: 100}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {ServiceDays: map[string]int{"smtp": 90}},
	}}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: hoursForDays(30),
		Targets:        []HostService{{Host: "web-01", Service: "smtp"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.DeletedCount != 0 || len(resp.DeletedTargets) != 0 {
		t.Errorf("deleted_count/targets = %d/%v, want 0/empty", resp.DeletedCount, resp.DeletedTargets)
	}
	if len(resp.BlockedTargets) != 1 {
		t.Errorf("blocked_targets = %+v, want one entry", resp.BlockedTargets)
	}
	if len(s.deletedWith) != 0 || len(s.countedWith) != 0 {
		t.Error("the store must never be called when every requested target is blocked")
	}
	// Regression check for the production "stuck loading" bug: decoding
	// through json.Unmarshal above can't tell a JSON `null` apart from
	// `[]` (both land as a nil/zero-length Go slice), which is exactly
	// how this shipped broken the first time -- deleted_targets has no
	// omitempty tag, so it must be a literal `[]` on the wire, not
	// `null`, or the frontend's `.length` access on it throws.
	if bytes.Contains(rec.Body.Bytes(), []byte(`"deleted_targets":null`)) {
		t.Errorf("deleted_targets marshaled as null, not []: %s", rec.Body.String())
	}
}

// TestHostsWithNoResultsReturnsEmptyArrayNotNull is a regression test
// for the production bug where a freshly-deployed instance (nothing yet
// old enough to be listed) got back `"hosts":null` instead of
// `"hosts":[]` -- Hosts has no omitempty tag, so the frontend's
// `hosts.length` threw mid-render on the null instead of the empty
// state ever painting. See handleHosts' hosts := []hostEntry{} comment.
func TestHostsWithNoResultsReturnsEmptyArrayNotNull(t *testing.T) {
	s := &fakeStore{targetList: nil}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=720")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"hosts":null`)) {
		t.Errorf("hosts marshaled as null, not []: %s", rec.Body.String())
	}
	var resp hostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Hosts) != 0 {
		t.Errorf("hosts = %+v, want empty", resp.Hosts)
	}
}

// TestPreviewAllTargetsBlockedReturnsEmptyArrayNotNull mirrors
// TestAllTargetsBlockedReturnsZeroCountNotError but for the preview
// endpoint, whose Targets field carries the identical no-omitempty risk.
func TestPreviewAllTargetsBlockedReturnsEmptyArrayNotNull(t *testing.T) {
	s := &fakeStore{count: 100}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {ServiceDays: map[string]int{"smtp": 90}},
	}}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{
		OlderThanHours: hoursForDays(30),
		Targets:        []HostService{{Host: "web-01", Service: "smtp"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"targets":null`)) {
		t.Errorf("targets marshaled as null, not []: %s", rec.Body.String())
	}
}

// TestOwnerBypassesRetentionFloor confirms the whole point of the
// feature: an owner can still delete within a configured retention
// window that blocks everyone else.
func TestOwnerBypassesRetentionFloor(t *testing.T) {
	s := &fakeStore{count: 100}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]HostFloor{
		"web-01": {ServiceDays: map[string]int{"smtp": 90}},
	}}, fakeAuthorizer{role: authz.RoleOwner})

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: hoursForDays(1),
		Targets:        []HostService{{Host: "web-01", Service: "smtp"}},
	})
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	want := []HostService{{Host: "web-01", Service: "smtp"}}
	if !reflect.DeepEqual(resp.DeletedTargets, want) {
		t.Errorf("deleted_targets = %v, want %v (owner bypasses the floor entirely)", resp.DeletedTargets, want)
	}
}

// TestNoConfiguredFloorNeverBlocksAdmin confirms the default, common
// case (no agent has any retention floor set) behaves exactly as
// before this feature existed.
func TestNoConfiguredFloorNeverBlocksAdmin(t *testing.T) {
	s := &fakeStore{count: 9}
	h := NewHandler(discardLogger(), s, fakeFloor{}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doJSONRequest(t, h, "POST", "/logs/retention/delete", deletionRequest{
		OlderThanHours: 1,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no configured floor", rec.Code)
	}
}

func TestRetentionFloorCheckPropagatesStoreErrors(t *testing.T) {
	s := &fakeStore{}
	h := NewHandler(discardLogger(), s, fakeFloor{err: errors.New("postgres unreachable")}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doJSONRequest(t, h, "POST", "/logs/retention/preview", deletionRequest{
		OlderThanHours: 24,
		Targets:        []HostService{{Host: "web-01", Service: "nginx"}},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
