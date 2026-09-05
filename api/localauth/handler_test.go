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

	"github.com/cairnobs/cairnobs/api/authz"
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

func TestResetPasswordAcceptsCallerSuppliedPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	bob := mustCreateUser(t, fs, "bob", "bobspassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	adminLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	adminCookie := sessionCookieFrom(adminLogin)

	reset := doRequest(t, mux, http.MethodPost, "/auth/users/"+bob.ID+"/reset-password", `{"password":"bobs-new-password"}`, adminCookie)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200; body=%s", reset.Code, reset.Body.String())
	}
	var resp resetPasswordResponse
	if err := json.Unmarshal(reset.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Password != "" {
		t.Errorf("expected no password echoed back when the caller supplied one, got %q", resp.Password)
	}

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"bob","password":"bobs-new-password"}`, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login with caller-supplied password: status = %d, want 200", login.Code)
	}
}

func TestOwnerCanReassignRole(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	bob := mustCreateUser(t, fs, "bob", "bobspassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	adminLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	adminCookie := sessionCookieFrom(adminLogin)
	bobLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"bob","password":"bobspassword"}`, nil)
	bobCookie := sessionCookieFrom(bobLogin)

	set := doRequest(t, mux, http.MethodPut, "/auth/users/"+bob.ID+"/role", `{"role":"admin"}`, adminCookie)
	if set.Code != http.StatusOK {
		t.Fatalf("set role status = %d, want 200; body=%s", set.Code, set.Body.String())
	}
	var updated userResponse
	if err := json.Unmarshal(set.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if updated.Role != "admin" {
		t.Errorf("role = %q, want admin", updated.Role)
	}

	stale := doRequest(t, mux, http.MethodGet, "/auth/session", "", bobCookie)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("bob's pre-reassignment session status = %d, want 401 (role change must revoke existing sessions)", stale.Code)
	}
}

// TestOwnerCanReassignEveryRoleTransition exercises every ordered pair
// of the four roles (viewer/editor/admin/owner), including a role's
// no-op transition to itself -- "an owner can reassign a role" must
// hold universally, not just for the one viewer->admin pair
// TestOwnerCanReassignRole already covers, and in particular must not
// silently special-case promotion to/from owner.
func TestOwnerCanReassignEveryRoleTransition(t *testing.T) {
	allRoles := []authz.Role{authz.RoleViewer, authz.RoleEditor, authz.RoleAdmin, authz.RoleOwner}

	for _, from := range allRoles {
		for _, to := range allRoles {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				fs := newFakeStore()
				mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
				target := mustCreateUser(t, fs, "target", "targetspassword", from)
				_, mux := newTestHandler(t, fs)

				adminLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
				adminCookie := sessionCookieFrom(adminLogin)

				set := doRequest(t, mux, http.MethodPut, "/auth/users/"+target.ID+"/role", `{"role":"`+string(to)+`"}`, adminCookie)
				if set.Code != http.StatusOK {
					t.Fatalf("set role %s -> %s: status = %d, want 200; body=%s", from, to, set.Code, set.Body.String())
				}
				var updated userResponse
				if err := json.Unmarshal(set.Body.Bytes(), &updated); err != nil {
					t.Fatalf("decoding response: %v", err)
				}
				if updated.Role != string(to) {
					t.Fatalf("role in response = %q, want %q", updated.Role, to)
				}

				// Confirm it actually took, not just that the handler said
				// so -- log back in as target and check the session's role.
				login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"target","password":"targetspassword"}`, nil)
				if login.Code != http.StatusOK {
					t.Fatalf("login as target after reassignment: status = %d", login.Code)
				}
				var loginResp sessionResponse
				if err := json.Unmarshal(login.Body.Bytes(), &loginResp); err != nil {
					t.Fatalf("decoding login response: %v", err)
				}
				if loginResp.Role != string(to) {
					t.Fatalf("role after fresh login = %q, want %q", loginResp.Role, to)
				}
			})
		}
	}
}

// TestOwnerCanReassignOwnRole confirms self-reassignment isn't
// special-cased away -- consistent with handleDeleteUser's documented
// "single-operator deployment knows what it's doing" trust posture, an
// owner can demote (or re-promote) themselves same as anyone else.
func TestOwnerCanReassignOwnRole(t *testing.T) {
	fs := newFakeStore()
	admin := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	// A second owner so demoting "admin" doesn't trip the "at least one
	// owner" guard this test isn't exercising.
	mustCreateUser(t, fs, "otherowner", "otherpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	set := doRequest(t, mux, http.MethodPut, "/auth/users/"+admin.ID+"/role", `{"role":"viewer"}`, cookie)
	if set.Code != http.StatusOK {
		t.Fatalf("self role change status = %d, want 200; body=%s", set.Code, set.Body.String())
	}
	var updated userResponse
	if err := json.Unmarshal(set.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if updated.Role != "viewer" {
		t.Errorf("role = %q, want viewer", updated.Role)
	}

	// The role change revokes sessions same as any other target -- the
	// admin's own now-stale cookie must stop working too.
	stale := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("own session after self-reassignment status = %d, want 401", stale.Code)
	}
}

func TestSetRoleRejectsInvalidRole(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	bob := mustCreateUser(t, fs, "bob", "bobspassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	adminLogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	adminCookie := sessionCookieFrom(adminLogin)

	rec := doRequest(t, mux, http.MethodPut, "/auth/users/"+bob.ID+"/role", `{"role":"superuser"}`, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid role", rec.Code)
	}
}

func TestNonOwnerCannotReassignRole(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	bob := mustCreateUser(t, fs, "bob", "bobspassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"hunter22"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPut, "/auth/users/"+bob.ID+"/role", `{"role":"admin"}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-owner reassigning a role", rec.Code)
	}
}

// --- RBAC matrix: admin's create/delete scope is viewer/editor only ---

func TestAdminCanListUsers(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"dave","password":"davespassword"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodGet, "/auth/users", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin listing users: status = %d, want 200", rec.Code)
	}
}

func TestAdminCanCreateViewerAndEditorOnly(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"dave","password":"davespassword"}`, nil)
	cookie := sessionCookieFrom(login)

	for _, role := range []string{"viewer", "editor"} {
		rec := doRequest(t, mux, http.MethodPost, "/auth/users",
			`{"username":"new-`+role+`","password":"somepassword1","role":"`+role+`"}`, cookie)
		if rec.Code != http.StatusCreated {
			t.Errorf("admin creating %s: status = %d, want 201, body=%s", role, rec.Code, rec.Body.String())
		}
	}
	for _, role := range []string{"admin", "owner"} {
		rec := doRequest(t, mux, http.MethodPost, "/auth/users",
			`{"username":"new-`+role+`","password":"somepassword1","role":"`+role+`"}`, cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("admin creating %s: status = %d, want 403", role, rec.Code)
		}
	}
}

func TestOwnerCanCreateAnyRole(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	for _, role := range []string{"viewer", "editor", "admin", "owner"} {
		rec := doRequest(t, mux, http.MethodPost, "/auth/users",
			`{"username":"new-`+role+`","password":"somepassword1","role":"`+role+`"}`, cookie)
		if rec.Code != http.StatusCreated {
			t.Errorf("owner creating %s: status = %d, want 201, body=%s", role, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminCanDeleteViewerAndEditorOnly(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	viewer := mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	editor := mustCreateUser(t, fs, "edith", "edithpassword", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"dave","password":"davespassword"}`, nil)
	cookie := sessionCookieFrom(login)

	for _, target := range []*User{viewer, editor} {
		rec := doRequest(t, mux, http.MethodDelete, "/auth/users/"+target.ID, "", cookie)
		if rec.Code != http.StatusNoContent {
			t.Errorf("admin deleting %s: status = %d, want 204, body=%s", target.Role, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminCannotDeleteAdminOrOwner(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	otherAdmin := mustCreateUser(t, fs, "dan", "danspassword", authz.RoleAdmin)
	owner := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"dave","password":"davespassword"}`, nil)
	cookie := sessionCookieFrom(login)

	for _, target := range []*User{otherAdmin, owner} {
		rec := doRequest(t, mux, http.MethodDelete, "/auth/users/"+target.ID, "", cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("admin deleting %s: status = %d, want 403", target.Role, rec.Code)
		}
	}
}

// --- RBAC matrix: at least one owner must always remain ---

func TestCannotDeleteTheLastOwner(t *testing.T) {
	fs := newFakeStore()
	owner := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodDelete, "/auth/users/"+owner.ID, "", cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting the last owner: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
}

func TestCanDeleteAnOwnerWhenAnotherRemains(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin1", "adminpass1", authz.RoleOwner)
	// The caller deletes the *other* owner, not itself. This test used
	// to sign in as admin1 and delete admin1, which passed only because
	// self-deletion was unguarded -- it asserted the lockout as if it
	// were the intended behaviour. What it means to test is that the
	// last-owner guard doesn't fire while a second owner remains, and
	// that holds without deleting the caller.
	owner2 := mustCreateUser(t, fs, "admin2", "adminpass2", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin1","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodDelete, "/auth/users/"+owner2.ID, "", cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleting one of two owners: status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
}

// The lockout this guards against: an owner creates a second owner,
// deletes their own account, and is signed out by the resulting
// local_sessions cascade with no supported way back in unless they
// already know the other account's password.
func TestCannotDeleteYourOwnAccount(t *testing.T) {
	fs := newFakeStore()
	owner := mustCreateUser(t, fs, "admin1", "adminpass1", authz.RoleOwner)
	// A second owner exists, so the last-owner guard is satisfied and
	// cannot be what refuses this.
	mustCreateUser(t, fs, "admin2", "adminpass2", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin1","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodDelete, "/auth/users/"+owner.ID, "", cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting your own account: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	// Still signed in, and the account still exists.
	sess := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if sess.Code != http.StatusOK {
		t.Fatalf("session after a refused self-delete: status = %d, want 200, body=%s", sess.Code, sess.Body.String())
	}
}

func TestCannotDemoteTheLastOwner(t *testing.T) {
	fs := newFakeStore()
	owner := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPut, "/auth/users/"+owner.ID+"/role", `{"role":"admin"}`, cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demoting the last owner: status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
}

// --- RBAC matrix: password resets on behalf of another user ---

func TestAdminCanResetNonOwnerPasswordsButNotOwners(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "dave", "davespassword", authz.RoleAdmin)
	viewer := mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	otherAdmin := mustCreateUser(t, fs, "dan", "danspassword", authz.RoleAdmin)
	owner := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"dave","password":"davespassword"}`, nil)
	cookie := sessionCookieFrom(login)

	for _, target := range []*User{viewer, otherAdmin} {
		rec := doRequest(t, mux, http.MethodPost, "/auth/users/"+target.ID+"/reset-password", "", cookie)
		if rec.Code != http.StatusOK {
			t.Errorf("admin resetting %s's password: status = %d, want 200, body=%s", target.Role, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, mux, http.MethodPost, "/auth/users/"+owner.ID+"/reset-password", "", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin resetting an owner's password: status = %d, want 403", rec.Code)
	}
}

func TestOwnerCanResetAnyPasswordIncludingAnotherOwner(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "admin1", "adminpass1", authz.RoleOwner)
	owner2 := mustCreateUser(t, fs, "admin2", "adminpass2", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin1","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/users/"+owner2.ID+"/reset-password", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner resetting another owner's password: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestResetPasswordRejectsTargetingSelf(t *testing.T) {
	fs := newFakeStore()
	owner := mustCreateUser(t, fs, "admin", "adminpass1", authz.RoleOwner)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"admin","password":"adminpass1"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/users/"+owner.ID+"/reset-password", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("resetting your own password via the admin endpoint: status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// --- self-service password change (POST /auth/password) ---

func TestChangeOwnPasswordSucceedsWithCorrectCurrentPassword(t *testing.T) {
	fs := newFakeStore()
	// Deliberately RoleViewer -- self-service must work for every role,
	// not just owner/admin.
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vincepassword"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/password",
		`{"current_password":"vincepassword","new_password":"vinces-new-password"}`, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}

	// The old session must be revoked...
	stale := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if stale.Code != http.StatusUnauthorized {
		t.Errorf("session after password change: status = %d, want 401", stale.Code)
	}
	// ...and the new password must actually work.
	relogin := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vinces-new-password"}`, nil)
	if relogin.Code != http.StatusOK {
		t.Fatalf("login with new password: status = %d, want 200", relogin.Code)
	}
}

func TestChangeOwnPasswordRejectsWrongCurrentPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vincepassword"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/password",
		`{"current_password":"wrongpassword","new_password":"vinces-new-password"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	// The session must still be valid -- a rejected attempt is not a
	// password change.
	still := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if still.Code != http.StatusOK {
		t.Errorf("session after a rejected attempt: status = %d, want 200", still.Code)
	}
}

func TestChangeOwnPasswordRejectsShortNewPassword(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vincepassword"}`, nil)
	cookie := sessionCookieFrom(login)

	rec := doRequest(t, mux, http.MethodPost, "/auth/password",
		`{"current_password":"vincepassword","new_password":"short"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestChangeOwnPasswordRequiresAuth(t *testing.T) {
	fs := newFakeStore()
	_, mux := newTestHandler(t, fs)

	rec := doRequest(t, mux, http.MethodPost, "/auth/password",
		`{"current_password":"whatever","new_password":"somenewpassword"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 with no session", rec.Code)
	}
}

func TestSetTimezoneStoresAndReportsOnSession(t *testing.T) {
	fs := newFakeStore()
	// RoleViewer deliberately: a viewer is the role most likely to be
	// only ever reading logs, and reading logs is what this setting is
	// for -- if it needed a higher role it would be useless.
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vincepassword"}`, nil)
	cookie := sessionCookieFrom(login)

	// Defaults to UTC before anything is set.
	before := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if got := decodeSessionTimezone(t, before); got != "UTC" {
		t.Fatalf("initial session timezone = %q, want %q", got, "UTC")
	}

	rec := doRequest(t, mux, http.MethodPut, "/auth/timezone", `{"timezone":"America/New_York"}`, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}

	// The same session keeps working -- unlike a password or role
	// change, a rendering preference is no reason to sign anyone out.
	after := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if after.Code != http.StatusOK {
		t.Fatalf("session after timezone change: status = %d, want 200", after.Code)
	}
	if got := decodeSessionTimezone(t, after); got != "America/New_York" {
		t.Errorf("session timezone = %q, want %q", got, "America/New_York")
	}
}

func TestSetTimezoneRejectsInvalidZones(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	login := doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"vince","password":"vincepassword"}`, nil)
	cookie := sessionCookieFrom(login)

	cases := map[string]string{
		"empty":          `{"timezone":""}`,
		"not a zone":     `{"timezone":"Mars/Olympus_Mons"}`,
		"fixed offset":   `{"timezone":"-07:00"}`,
		"server-local":   `{"timezone":"Local"}`,
		"absurdly long":  `{"timezone":"` + strings.Repeat("x", 200) + `"}`,
		"path traversal": `{"timezone":"../../etc/passwd"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, mux, http.MethodPut, "/auth/timezone", body, cookie)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// Nothing above should have changed the stored value.
	sess := doRequest(t, mux, http.MethodGet, "/auth/session", "", cookie)
	if got := decodeSessionTimezone(t, sess); got != "UTC" {
		t.Errorf("timezone after rejected requests = %q, want %q", got, "UTC")
	}
}

func TestSetTimezoneRequiresAuth(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "vince", "vincepassword", authz.RoleViewer)
	_, mux := newTestHandler(t, fs)

	rec := doRequest(t, mux, http.MethodPut, "/auth/timezone", `{"timezone":"Europe/Berlin"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func decodeSessionTimezone(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding session body: %v", err)
	}
	return body.Timezone
}
