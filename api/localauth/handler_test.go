package localauth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sentry/sentry/api/authz"
)

func newTestHandler(t *testing.T, fs *fakeStore) (*Handler, *http.ServeMux) {
	t.Helper()
	authorizer := NewAuthorizer(fs)
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), fs, authorizer, time.Hour, CookieConfig{})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func mustCreateUser(t *testing.T, fs *fakeStore, username, password string, role authz.Role) *User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	u, err := fs.CreateUser(t.Context(), username, hash, role)
	if err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return u
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func TestLoginSuccess(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	rec := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"hunter22"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cookie := sessionCookieFrom(rec)
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("expected a session cookie to be set, got none")
	}
	if !cookie.HttpOnly {
		t.Errorf("session cookie must be HttpOnly")
	}

	var resp sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Token == "" {
		t.Errorf("expected the response body to also carry the raw token for non-browser callers")
	}
	if resp.Role != "editor" {
		t.Errorf("role = %q, want editor", resp.Role)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	rec := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"wrong"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLoginUnknownUserSameErrorAsWrongPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	unknown := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"bob","password":"whatever"}`, nil)
	wrongPass := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"wrong"}`, nil)

	if unknown.Code != http.StatusUnauthorized || wrongPass.Code != http.StatusUnauthorized {
		t.Fatalf("both must be 401, got unknown=%d wrongPass=%d", unknown.Code, wrongPass.Code)
	}
	if unknown.Body.String() != wrongPass.Body.String() {
		t.Errorf("responses must be identical (no username enumeration): unknown=%q wrongPass=%q", unknown.Body.String(), wrongPass.Body.String())
	}
}

func TestSessionRequiresAuth(t *testing.T) {
	fs := newFakeStore()
	_, mux := newTestHandler(t, fs)

	rec := doRequest(t, mux, http.MethodGet, "/auth/session", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 with no session cookie", rec.Code)
	}
}

func TestLoginThenSessionRoundTrip(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"hunter22"}`, nil)
	cookie := sessionCookieFrom(login)

	sess := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if sess.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", sess.Code, sess.Body.String())
	}
	var resp sessionResponse
	if err := json.Unmarshal(sess.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Username != "alice" {
		t.Errorf("username = %q, want alice", resp.Username)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"hunter22"}`, nil)
	cookie := sessionCookieFrom(login)

	logout := doRequest(t, mux, http.MethodPost, "/auth/logout", "", cookie)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logout.Code)
	}

	sess := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if sess.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d, want 401 (session must be revoked)", sess.Code)
	}
}

func TestNonOwnerCannotManageUsers(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"hunter22"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodGet, "/auth/users", "", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-owner listing users", rec.Code)
	}
}

func TestOwnerCanCreateAndDeleteUsers(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	create := doRequest(t, mux, http.MethodPost, "/auth/users", `{"username":"bob","password":"bobspassword","role":"viewer"}`, cookie)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", create.Code, create.Body.String())
	}
	var created userResponse
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if created.Role != "viewer" {
		t.Errorf("role = %q, want viewer", created.Role)
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("created_at was not populated in the create response")
	}

	list := doRequest(t, mux, http.MethodGet, "/auth/users", "", cookie)
	var users []userResponse
	if err := json.Unmarshal(list.Body.Bytes(), &users); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d, want 2 (admin + bob)", len(users))
	}

	del := doRequest(t, mux, http.MethodDelete, "/auth/users/"+created.ID, "", cookie)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", del.Code)
	}
}

func TestCreateUserRejectsShortPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/users", `{"username":"bob","password":"short"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a too-short password", rec.Code)
	}
}

func TestResetPasswordRevokesExistingSessions(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	bob := mustCreateUser(t, fs, "bob", "bobspassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	adminLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	adminCookie := sessionCookieFrom(adminLogin)
	bobLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"bob","password":"bobspassword"}`, nil)
	bobCookie := sessionCookieFrom(bobLogin)

	reset := doRequest(t, mux, http.MethodPost, "/auth/users/"+bob.ID+"/reset-password", "", adminCookie)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200; body=%s", reset.Code, reset.Body.String())
	}
	var resp resetPasswordResponse
	if err := json.Unmarshal(reset.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Password == "" {
		t.Fatalf("expected a generated password in the response when none was supplied")
	}

	stale := doRequest(t, mux, http.MethodGet, "/auth/session", "", bobCookie)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("bob's pre-reset session status = %d, want 401 (reset must revoke existing sessions)", stale.Code)
	}
}
