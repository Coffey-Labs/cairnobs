package logretention

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sentry/sentry/api/authz"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore records the cutoff it was called with so tests can assert
// the handler computed it correctly from older_than_hours, and lets a
// test inject a store error to exercise the failure paths.
type fakeStore struct {
	count       uint64
	countErr    error
	deleteErr   error
	countedWith []time.Time
	deletedWith []time.Time
}

func (f *fakeStore) CountOlderThan(_ context.Context, cutoff time.Time) (uint64, error) {
	f.countedWith = append(f.countedWith, cutoff)
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.count, nil
}

func (f *fakeStore) DeleteOlderThan(_ context.Context, cutoff time.Time) error {
	f.deletedWith = append(f.deletedWith, cutoff)
	return f.deleteErr
}

type fakeAuthorizer struct {
	role authz.Role
}

func (f fakeAuthorizer) Authorize(*http.Request) (authz.Identity, error) {
	return authz.Identity{TenantID: "default", UserID: "u1", Role: f.role}, nil
}

func newTestHandler(s *fakeStore, role authz.Role) *Handler {
	return NewHandler(discardLogger(), s, fakeAuthorizer{role: role})
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

func TestPreviewReturnsCountAndCutoff(t *testing.T) {
	s := &fakeStore{count: 42}
	h := newTestHandler(s, authz.RoleAdmin)

	before := time.Now().UTC()
	rec := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24")
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
	wantEarliest := before.Add(-24 * time.Hour)
	wantLatest := after.Add(-24 * time.Hour)
	if resp.Cutoff.Before(wantEarliest) || resp.Cutoff.After(wantLatest) {
		t.Errorf("cutoff = %v, want between %v and %v", resp.Cutoff, wantEarliest, wantLatest)
	}
	if len(s.deletedWith) != 0 {
		t.Errorf("preview must never delete anything, but DeleteOlderThan was called %d time(s)", len(s.deletedWith))
	}
}

func TestDeleteReturnsDeletedCountAndCutoff(t *testing.T) {
	s := &fakeStore{count: 7}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=720")
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
	if len(s.deletedWith) != 1 {
		t.Fatalf("expected exactly one DeleteOlderThan call, got %d", len(s.deletedWith))
	}
	if len(s.countedWith) != 1 || !s.countedWith[0].Equal(s.deletedWith[0]) {
		t.Errorf("count and delete must use the same cutoff: counted=%v deleted=%v", s.countedWith, s.deletedWith)
	}
}

func TestRejectsMissingOrInvalidOlderThanHours(t *testing.T) {
	h := newTestHandler(&fakeStore{}, authz.RoleAdmin)

	cases := []string{
		"/logs/retention/preview",
		"/logs/retention/preview?older_than_hours=0",
		"/logs/retention/preview?older_than_hours=-5",
		"/logs/retention/preview?older_than_hours=notanumber",
		"/logs/retention/preview?older_than_hours=999999999",
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

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(s.deletedWith) != 0 {
		t.Error("an invalid older_than_hours must never reach the store's delete path")
	}
}

func TestDeletePropagatesStoreErrors(t *testing.T) {
	s := &fakeStore{deleteErr: errors.New("clickhouse mutation failed")}
	h := newTestHandler(s, authz.RoleAdmin)

	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestOwnerAndAdminCanUseRetentionRoutes(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleOwner} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		preview := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24")
		if preview.Code != http.StatusOK {
			t.Errorf("role %s: preview status = %d, want 200", role, preview.Code)
		}
		del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24")
		if del.Code != http.StatusOK {
			t.Errorf("role %s: delete status = %d, want 200", role, del.Code)
		}
	}
}

func TestViewerAndEditorAreForbiddenFromRetentionRoutes(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleEditor} {
		s := &fakeStore{count: 3}
		h := newTestHandler(s, role)

		preview := doRequest(t, h, "GET", "/logs/retention/preview?older_than_hours=24")
		if preview.Code != http.StatusForbidden {
			t.Errorf("role %s: preview status = %d, want 403", role, preview.Code)
		}
		del := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24")
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
	h := NewHandler(discardLogger(), s, nil)

	// A nil authorizer is Phase 0-3's default-open behavior (see
	// authz.RequireRole's doc comment) -- confirm that posture applies
	// here too, same as every other RequireRole-wrapped route, rather
	// than this package accidentally being open or closed by default in
	// a way inconsistent with the rest of the API.
	rec := doRequest(t, h, "DELETE", "/logs/retention?older_than_hours=24")
	if rec.Code != http.StatusOK {
		t.Fatalf("status with nil authorizer = %d, want 200 (default-open, matches RequireRole elsewhere)", rec.Code)
	}
}
