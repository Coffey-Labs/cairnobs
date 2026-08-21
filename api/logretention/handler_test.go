package logretention

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sentry/sentry/api/authz"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// hostCall records one CountOlderThan/DeleteOlderThan invocation, so
// tests can assert both the cutoff and the exact host set a call used.
type hostCall struct {
	cutoff time.Time
	hosts  []string
}

// fakeStore lets a test inject store errors and a fixed host listing,
// and records every count/delete call it received so tests can assert
// the handler scoped them to the right hosts.
type fakeStore struct {
	hostList    []HostCount
	hostsErr    error
	count       uint64
	countErr    error
	deleteErr   error
	countedWith []hostCall
	deletedWith []hostCall
}

func (f *fakeStore) HostsOlderThan(_ context.Context, _ time.Time) ([]HostCount, error) {
	if f.hostsErr != nil {
		return nil, f.hostsErr
	}
	return f.hostList, nil
}

func (f *fakeStore) CountOlderThan(_ context.Context, cutoff time.Time, hosts []string) (uint64, error) {
	f.countedWith = append(f.countedWith, hostCall{cutoff, hosts})
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func (f *fakeStore) DeleteOlderThan(_ context.Context, cutoff time.Time, hosts []string) error {
	f.deletedWith = append(f.deletedWith, hostCall{cutoff, hosts})
	return f.deleteErr
}

type fakeAuthorizer struct {
	role authz.Role
}

func (f fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return authz.Identity{TenantID: "default", UserID: "u1", Role: f.role}, nil
}

// fakeFloor stands in for AgentRetentionStore -- a nil/empty byHost map
// means no agent has log_retention_days configured, same as every test
// that doesn't care about the floor assumed before it existed.
type fakeFloor struct {
	byHost map[string]int
	err    error
}

func (f fakeFloor) RetentionDaysByHost(context.Context) (map[string]int, error) {
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

func hoursForDays(days int) string {
	return strconv.Itoa(days * 24)
}

func TestHostsListsHostsWithCountsAndFloors(t *testing.T) {
	s := &fakeStore{hostList: []HostCount{{Host: "web-01", Count: 100}, {Host: "web-02", Count: 5}}}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]int{"web-02": 90}}, fakeAuthorizer{role: authz.RoleAdmin})

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
	if resp.Hosts[0].Host != "web-01" || resp.Hosts[0].Count != 100 || resp.Hosts[0].ProtectedDays != nil {
		t.Errorf("hosts[0] = %+v, want web-01/100/no floor", resp.Hosts[0])
	}
	if resp.Hosts[1].Host != "web-02" || resp.Hosts[1].Count != 5 || resp.Hosts[1].ProtectedDays == nil || *resp.Hosts[1].ProtectedDays != 90 {
		t.Errorf("hosts[1] = %+v, want web-02/5/floor=90", resp.Hosts[1])
	}
}

func TestPreviewReturnsCountCutoffAndHosts(t *testing.T) {
	s := &fakeStore{count: 42}
	h := newTestHandler(s, authz.RoleAdmin)

	before := time.Now().UTC()
	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=web-01&host=web-02")
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
	if !reflect.DeepEqual(resp.Hosts, []string{"web-01", "web-02"}) {
		t.Errorf("hosts = %v, want [web-01 web-02]", resp.Hosts)
	}
	wantEarliest := before.Add(-24 * time.Hour)
	wantLatest := after.Add(-24 * time.Hour)
	if resp.Cutoff.Before(wantEarliest) || resp.Cutoff.After(wantLatest) {
		t.Errorf("cutoff = %v, want between %v and %v", resp.Cutoff, wantEarliest, wantLatest)
	}
	if len(s.deletedWith) != 0 {
		t.Errorf("preview must never delete anything, but DeleteOlderThan was called %d time(s)", len(s.deletedWith))
	}
	if len(s.countedWith) != 1 || !reflect.DeepEqual(s.countedWith[0].hosts, []string{"web-01", "web-02"}) {
		t.Errorf("CountOlderThan was not scoped to the requested hosts: %+v", s.countedWith)
	}
}

func TestPreviewDedupesHosts(t *testing.T) {
	s := &fakeStore{count: 1}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=web-01&host=web-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(s.countedWith) != 1 || !reflect.DeepEqual(s.countedWith[0].hosts, []string{"web-01"}) {
		t.Fatalf("expected a deduped single-host call, got %+v", s.countedWith)
	}
}

func TestPreviewRequiresAtLeastOneHost(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no host specified", rec.Code)
	}
}

func TestPreviewRejectsEmptyHostValue(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with an empty host value", rec.Code)
	}
}

func TestDeleteReturnsDeletedCountHostsAndCutoff(t *testing.T) {
	s := &fakeStore{count: 7}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=720&host=web-01")
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
	if !reflect.DeepEqual(resp.DeletedHosts, []string{"web-01"}) {
		t.Errorf("deleted_hosts = %v, want [web-01]", resp.DeletedHosts)
	}
	if len(s.deletedWith) != 1 || !reflect.DeepEqual(s.deletedWith[0].hosts, []string{"web-01"}) {
		t.Fatalf("expected exactly one scoped DeleteOlderThan call, got %+v", s.deletedWith)
	}
	if len(s.countedWith) != 1 || !s.countedWith[0].cutoff.Equal(s.deletedWith[0].cutoff) {
		t.Errorf("count and delete must use the same cutoff: counted=%+v deleted=%+v", s.countedWith, s.deletedWith)
	}
}

func TestRejectsMissingOrInvalidOlderThanHours(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	cases := []string{
		"/logs/retention/preview?host=web-01",
		"/logs/retention/preview?older_than_hours=0&host=web-01",
		"/logs/retention/preview?older_than_hours=-5&host=web-01",
		"/logs/retention/preview?older_than_hours=notanumber&host=web-01",
		"/logs/retention/preview?older_than_hours=999999999&host=web-01",
	}
	for _, path := range cases {
		rec := doRequest(t, h, "GET", path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestDeleteRejectsInvalidOlderThanHours(t *testing.T) {
	s := &fakeStore{}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=0&host=web-01")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(s.deletedWith) != 0 {
		t.Error("an invalid older_than_hours must never reach the store's delete path")
	}
}

func TestDeleteRejectsMissingHosts(t *testing.T) {
	s := &fakeStore{}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no host specified", rec.Code)
	}
	if len(s.deletedWith) != 0 {
		t.Error("a request with no host specified must never reach the store's delete path")
	}
}

func TestDeletePropagatesStoreErrors(t *testing.T) {
	s := &fakeStore{deleteErr: errors.New("clickhouse mutation failed")}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24&host=web-01")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestOwnerAndAdminCanUseRetentionRoutes(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleOwner} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		hosts := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=24")
		if hosts.Code != http.StatusOK {
			t.Errorf("role %s: hosts status = %d, want 200", role, hosts.Code)
		}
		preview := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=web-01")
		if preview.Code != http.StatusOK {
			t.Errorf("role %s: preview status = %d, want 200", role, preview.Code)
		}
		del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24&host=web-01")
		if del.Code != http.StatusOK {
			t.Errorf("role %s: delete status = %d, want 200", role, del.Code)
		}
	}
}

func TestViewerAndEditorAreForbiddenFromRetentionRoutes(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleEditor} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		hosts := doRequest(t, h, "GET", "/logs/retention/hosts?older_than_hours=24")
		if hosts.Code != http.StatusForbidden {
			t.Errorf("role %s: hosts status = %d, want 403", role, hosts.Code)
		}
		preview := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=web-01")
		if preview.Code != http.StatusForbidden {
			t.Errorf("role %s: preview status = %d, want 403", role, preview.Code)
		}
		del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24&host=web-01")
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
	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24&host=web-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("status with nil authorizer = %d, want 200 (default-open, matches RequireRole elsewhere)", rec.Code)
	}
}

// TestAdminPartiallyBlockedByPerHostRetentionFloor is the core
// regression test for host-scoped floor enforcement: requesting two
// hosts where only one has a protective floor must delete the
// unprotected host and report the other as blocked, not reject the
// whole request.
func TestAdminPartiallyBlockedByPerHostRetentionFloor(t *testing.T) {
	s := &fakeStore{count: 5}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]int{"protected-host": 90}}, fakeAuthorizer{role: authz.RoleAdmin})

	// 30 days is newer than protected-host's 90-day floor.
	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours="+hoursForDays(30)+"&host=protected-host&host=open-host")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial success, not an error), body=%s", rec.Code, rec.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !reflect.DeepEqual(resp.DeletedHosts, []string{"open-host"}) {
		t.Errorf("deleted_hosts = %v, want [open-host]", resp.DeletedHosts)
	}
	if len(resp.BlockedHosts) != 1 || resp.BlockedHosts[0].Host != "protected-host" || resp.BlockedHosts[0].ProtectedDays != 90 {
		t.Errorf("blocked_hosts = %+v, want [{protected-host 90}]", resp.BlockedHosts)
	}
	if len(s.deletedWith) != 1 || !reflect.DeepEqual(s.deletedWith[0].hosts, []string{"open-host"}) {
		t.Fatalf("DeleteOlderThan must only ever be scoped to the allowed host, got %+v", s.deletedWith)
	}
}

// TestAllHostsBlockedReturnsZeroCountNotError confirms a request where
// every requested host is protected still succeeds (200), just with
// nothing deleted -- informative, not an error condition, since the
// request itself was perfectly valid.
func TestAllHostsBlockedReturnsZeroCountNotError(t *testing.T) {
	s := &fakeStore{count: 100}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]int{"protected-host": 90}}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours="+hoursForDays(30)+"&host=protected-host")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.DeletedCount != 0 {
		t.Errorf("deleted_count = %d, want 0", resp.DeletedCount)
	}
	if len(resp.DeletedHosts) != 0 {
		t.Errorf("deleted_hosts = %v, want empty", resp.DeletedHosts)
	}
	if len(resp.BlockedHosts) != 1 || resp.BlockedHosts[0].Host != "protected-host" {
		t.Errorf("blocked_hosts = %+v, want [{protected-host 90}]", resp.BlockedHosts)
	}
	if len(s.deletedWith) != 0 || len(s.countedWith) != 0 {
		t.Error("the store must never be called when every requested host is blocked")
	}
}

// TestAdminAllowedBeyondRetentionFloor confirms the floor only blocks
// requests that would actually reach into the protected window -- a
// request older than the floor itself is unaffected by it.
func TestAdminAllowedBeyondRetentionFloor(t *testing.T) {
	s := &fakeStore{count: 5}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]int{"web-01": 90}}, fakeAuthorizer{role: authz.RoleAdmin})

	// 120 days is older than the 90-day floor -- must be allowed.
	del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours="+hoursForDays(120)+"&host=web-01")
	if del.Code != http.StatusOK {
		t.Fatalf("delete at 120d against a 90d floor: status = %d, want 200, body=%s", del.Code, del.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(del.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.BlockedHosts) != 0 {
		t.Errorf("blocked_hosts = %+v, want none", resp.BlockedHosts)
	}
}

// TestOwnerBypassesRetentionFloor confirms the whole point of the
// feature: an owner can still delete within a configured retention
// window that blocks everyone else.
func TestOwnerBypassesRetentionFloor(t *testing.T) {
	s := &fakeStore{count: 100}
	h := NewHandler(discardLogger(), s, fakeFloor{byHost: map[string]int{"web-01": 90}}, fakeAuthorizer{role: authz.RoleOwner})

	del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours="+hoursForDays(1)+"&host=web-01")
	if del.Code != http.StatusOK {
		t.Fatalf("owner deleting within the floor: status = %d, want 200, body=%s", del.Code, del.Body.String())
	}
	var resp deleteResponse
	if err := json.Unmarshal(del.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !reflect.DeepEqual(resp.DeletedHosts, []string{"web-01"}) {
		t.Errorf("deleted_hosts = %v, want [web-01] (owner bypasses the floor entirely)", resp.DeletedHosts)
	}
}

// TestNoConfiguredFloorNeverBlocksAdmin confirms the default, common
// case (no agent has log_retention_days set) behaves exactly as before
// this feature existed.
func TestNoConfiguredFloorNeverBlocksAdmin(t *testing.T) {
	s := &fakeStore{count: 9}
	h := NewHandler(discardLogger(), s, fakeFloor{}, fakeAuthorizer{role: authz.RoleAdmin})

	del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=1&host=web-01")
	if del.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no configured floor", del.Code)
	}
}

func TestRetentionFloorCheckPropagatesStoreErrors(t *testing.T) {
	s := &fakeStore{}
	h := NewHandler(discardLogger(), s, fakeFloor{err: errors.New("postgres unreachable")}, fakeAuthorizer{role: authz.RoleAdmin})

	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24&host=web-01")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
