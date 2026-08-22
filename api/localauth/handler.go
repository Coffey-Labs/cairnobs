package localauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cairnobs/cairnobs/api/authz"
)

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as queryapi/dashboards/agents

// store is the narrow interface Handler depends on -- *Store (store.go)
// is the production implementation; tests use a fake, same pattern as
// dashboards.store/agents.store.
type store interface {
	CreateUser(ctx context.Context, username, passwordHash string, role authz.Role) (*User, error)
	ListUsers(ctx context.Context) ([]User, error)
	GetUserForLogin(ctx context.Context, username string) (*User, string, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetPasswordHashByID(ctx context.Context, id string) (string, error)
	DeleteUser(ctx context.Context, id string) error
	SetPasswordHash(ctx context.Context, userID, hash string) error
	SetRole(ctx context.Context, userID string, role authz.Role) error
	SetDisplayTimezone(ctx context.Context, userID, tz string) error
	CountUsersWithRole(ctx context.Context, role authz.Role) (int, error)
	CreateSession(ctx context.Context, userID, tenantID string, role authz.Role, ttl time.Duration) (string, error)
	DeleteSessionByHash(ctx context.Context, tokenHash string) error
}

// CookieConfig is the deployment-specific half of how the session
// cookie is set -- everything else about it (name, HttpOnly, SameSite)
// is fixed by this package, not configurable per deployment.
type CookieConfig struct {
	// Domain is typically empty for local dev (host-only cookie, works
	// fine when web/api are both localhost:<port>) and something like
	// ".cairnobs.example.com" in production, so the same cookie is sent to
	// api.cairnobs.example.com and alerting.cairnobs.example.com too -- see
	// /docs (deployment runbook) for the subdomain scheme this assumes.
	Domain string
	// Secure defaults to true (the cookie is never sent over plain
	// HTTP) -- deliberately opt-out, not opt-in, since the real
	// deployment this feature exists for is always behind HTTPS. Only
	// worth setting false to test the login flow locally over plain
	// http://localhost.
	Secure bool
}

// loginRateLimitMax/Window bound how many login attempts one client IP
// may make -- see loginLimiter's doc comment for why this is per-IP,
// in-memory, and counts both successful and failed attempts. 10 per 5
// minutes is generous enough that a real user mistyping a password a
// few times never notices, while still bounding an online brute-force
// attempt to a few attempts per minute.
const (
	loginRateLimitMax    = 10
	loginRateLimitWindow = 5 * time.Minute
)

type Handler struct {
	logger      *slog.Logger
	store       store
	authorizer  authz.Authorizer
	sessionTTL  time.Duration
	cookies     CookieConfig
	loginLimits *loginLimiter
}

func NewHandler(logger *slog.Logger, store store, authorizer authz.Authorizer, sessionTTL time.Duration, cookies CookieConfig) *Handler {
	return &Handler{
		logger:      logger,
		store:       store,
		authorizer:  authorizer,
		sessionTTL:  sessionTTL,
		cookies:     cookies,
		loginLimits: newLoginLimiter(loginRateLimitMax, loginRateLimitWindow),
	}
}

// RegisterRoutes is only ever called when local auth is enabled (see
// cmd/api/main.go) -- a deployment that doesn't enable it simply never
// registers these routes at all, so GET /auth/session (etc.) 404s
// rather than needing its own "is this feature even on" response
// shape. Login/logout/session/password are deliberately NOT
// RequireRole-wrapped with anything above RoleViewer's floor: login is
// how you become authenticated in the first place, logout/session/
// changing your own password must work for any already-authenticated
// user regardless of role.
//
// User management's RBAC matrix (each floor here is the minimum to
// reach the route at all; every handler below applies a further,
// per-target check narrower than its floor):
//   - GET/POST /auth/users, DELETE .../{id}, POST .../reset-password:
//     RoleAdmin floor. An admin caller is then restricted to
//     viewer/editor targets only (handleCreateUser/handleDeleteUser/
//     handleResetPassword); an owner caller has no such restriction.
//   - DELETE .../{id} additionally refuses to remove the last owner,
//     and POST .../reset-password refuses id == the caller's own ID
//     (see POST /auth/password) and refuses an owner target unless the
//     caller is themselves an owner.
//   - PUT .../{id}/role stays RoleOwner-only (unchanged) but now also
//     refuses to demote the last owner.
//   - POST /auth/password (self-service, RoleViewer floor) is the only
//     way to change your own password, verified against your current
//     one -- this applies to every role including owner, since "an
//     owner can change any password on behalf of a user" is about
//     acting on someone ELSE's account, not a shortcut around
//     confirming your own current password.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/login", h.handleLogin)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
	mux.HandleFunc("GET /auth/session", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleGetSession))
	mux.HandleFunc("POST /auth/password", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleChangeOwnPassword))
	// Self-service, same RoleViewer floor as the password change above:
	// how a user's own clock is rendered is nobody else's permission to
	// grant, and a Viewer -- the role most likely to be *only* reading
	// logs -- needs it most.
	mux.HandleFunc("PUT /auth/timezone", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleSetTimezone))

	mux.HandleFunc("GET /auth/users", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleListUsers))
	mux.HandleFunc("POST /auth/users", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleCreateUser))
	mux.HandleFunc("DELETE /auth/users/{id}", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleDeleteUser))
	mux.HandleFunc("POST /auth/users/{id}/reset-password", authz.RequireRole(h.authorizer, authz.RoleAdmin, h.handleResetPassword))
	mux.HandleFunc("PUT /auth/users/{id}/role", authz.RequireRole(h.authorizer, authz.RoleOwner, h.handleSetRole))
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionResponse struct {
	// Token duplicates what the Set-Cookie header already carries,
	// specifically for non-browser callers with no cookie jar --
	// cairnobsctl captures this into CAIRNOBSCTL_TOKEN and sends it back as
	// Authorization: Bearer (see authorizer.go's credentialFromRequest,
	// which accepts either). The web UI ignores this field entirely and
	// relies on the cookie.
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	// Timezone is the user's stored display preference (IANA zone name,
	// "UTC" by default). Sent on the session so the web UI knows which
	// offset to render in from its very first paint, without a second
	// round trip -- omitted from the login response, where the store
	// lookup that produces it hasn't happened.
	Timezone string `json:"timezone,omitempty"`
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.loginLimits.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}

	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	user, hash, err := h.store.GetUserForLogin(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Run a dummy bcrypt comparison even though there's no real
			// hash to check -- otherwise this branch returns immediately
			// while a known-username branch always pays bcrypt's cost
			// below, and that timing gap lets a patient caller enumerate
			// valid usernames by response latency alone even though the
			// error message text is identical either way.
			ComparePassword(dummyPasswordHash, req.Password)
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		h.logger.Error("looking up user for login", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	if !ComparePassword(hash, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	raw, err := h.store.CreateSession(r.Context(), user.ID, defaultTenantID, user.Role, h.sessionTTL)
	if err != nil {
		h.logger.Error("creating session", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	h.setCookie(w, raw, h.sessionTTL)
	writeJSON(w, http.StatusOK, sessionResponse{
		Token: raw, UserID: user.ID, TenantID: defaultTenantID,
		Username: user.Username, Role: string(user.Role),
	})
}

// handleLogout always responds 204, whether or not a valid session was
// presented -- "log me out" is idempotent from the caller's point of
// view either way.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if raw, err := credentialFromRequest(r); err == nil {
		if err := h.store.DeleteSessionByHash(r.Context(), hashToken(raw)); err != nil {
			h.logger.Error("deleting session", "error", err)
		}
	}
	h.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSession is what web's route guard (+layout.ts) polls on
// every navigation -- RequireRole(RoleViewer) above already turns "no
// valid session" into a 401 before this ever runs, so by the time
// we're here the identity is real.
func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	identity, _ := authz.IdentityFromContext(r.Context())
	user, err := h.store.GetUserByID(r.Context(), identity.UserID)
	if err != nil {
		h.writeStoreErr(w, err, "fetching session user")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		UserID: user.ID, TenantID: identity.TenantID, Username: user.Username, Role: string(user.Role),
		Timezone: user.DisplayTimezone,
	})
}

type userResponse struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("listing users", "error", err)
		writeError(w, http.StatusInternalServerError, "listing users failed")
		return
	}
	out := make([]userResponse, len(users))
	for i, u := range users {
		out[i] = userResponse{ID: u.ID, Username: u.Username, Role: string(u.Role), CreatedAt: u.CreatedAt}
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func validRole(r authz.Role) bool {
	switch r {
	case authz.RoleViewer, authz.RoleEditor, authz.RoleAdmin, authz.RoleOwner:
		return true
	default:
		return false
	}
}

func (h *Handler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" {
		writeError(w, http.StatusBadRequest, "username must not be empty")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	role := authz.Role(req.Role)
	if role == "" {
		role = authz.RoleEditor
	}
	if !validRole(role) {
		writeError(w, http.StatusBadRequest, `role must be "viewer", "editor", "admin", or "owner"`)
		return
	}
	// An admin caller (RegisterRoutes' RoleAdmin floor already excludes
	// viewer/editor) may only create viewer/editor accounts -- creating
	// an admin or owner account is owner-only.
	if identity, ok := authz.IdentityFromContext(r.Context()); ok && !identity.Role.Satisfies(authz.RoleOwner) {
		if role == authz.RoleAdmin || role == authz.RoleOwner {
			writeError(w, http.StatusForbidden, "admin can only create viewer or editor accounts")
			return
		}
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		h.logger.Error("hashing password", "error", err)
		writeError(w, http.StatusInternalServerError, "creating user failed")
		return
	}
	user, err := h.store.CreateUser(r.Context(), req.Username, hash, role)
	if err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			writeError(w, http.StatusConflict, "username already taken")
			return
		}
		h.logger.Error("creating user", "error", err)
		writeError(w, http.StatusInternalServerError, "creating user failed")
		return
	}
	writeJSON(w, http.StatusCreated, userResponse{ID: user.ID, Username: user.Username, Role: string(user.Role), CreatedAt: user.CreatedAt})
}

// handleDeleteUser deliberately does not stop a caller from deleting
// their own account (as long as it isn't the last owner, per the check
// below) -- a single-operator prototype deployment is expected to know
// what it's doing here, same trust level the rest of this codebase's
// admin-only endpoints assume. What it does stop, unconditionally: an
// admin caller (RegisterRoutes' RoleAdmin floor already excludes
// viewer/editor) deleting an admin or owner target -- that's
// owner-only -- and anyone at all deleting the last remaining owner.
func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "deleting user")
		return
	}

	if identity, ok := authz.IdentityFromContext(r.Context()); ok && !identity.Role.Satisfies(authz.RoleOwner) {
		if target.Role == authz.RoleAdmin || target.Role == authz.RoleOwner {
			writeError(w, http.StatusForbidden, "admin can only delete viewer or editor accounts")
			return
		}
	}
	if target.Role == authz.RoleOwner {
		if blocked, err := h.wouldRemoveLastOwner(r.Context(), w); err != nil {
			return
		} else if blocked {
			writeError(w, http.StatusConflict, "cannot delete the last owner")
			return
		}
	}

	if err := h.store.DeleteUser(r.Context(), id); err != nil {
		h.writeStoreErr(w, err, "deleting user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// wouldRemoveLastOwner reports whether the default tenant currently has
// only one owner -- shared by handleDeleteUser (deleting that owner)
// and handleSetRole (demoting them) since both are exactly the same
// invariant violation from two different operations. Writes a 500 and
// returns a non-nil error itself on a store failure, so callers can
// just `return` on a non-nil error without writing their own.
func (h *Handler) wouldRemoveLastOwner(ctx context.Context, w http.ResponseWriter) (bool, error) {
	n, err := h.store.CountUsersWithRole(ctx, authz.RoleOwner)
	if err != nil {
		h.logger.Error("counting owners", "error", err)
		writeError(w, http.StatusInternalServerError, "checking owner count failed")
		return false, err
	}
	return n <= 1, nil
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// handleSetRole deliberately does not stop an owner from demoting or
// re-promoting their own account -- same "single-operator prototype
// deployment knows what it's doing" trust level handleDeleteUser's doc
// comment already establishes for this package's owner-only endpoints.
// It does stop demoting the last remaining owner away from RoleOwner,
// the same invariant handleDeleteUser enforces for deletion.
func (h *Handler) handleSetRole(w http.ResponseWriter, r *http.Request) {
	var req setRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	role := authz.Role(req.Role)
	if !validRole(role) {
		writeError(w, http.StatusBadRequest, `role must be "viewer", "editor", "admin", or "owner"`)
		return
	}

	id := r.PathValue("id")
	current, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "updating role")
		return
	}
	if current.Role == authz.RoleOwner && role != authz.RoleOwner {
		if blocked, err := h.wouldRemoveLastOwner(r.Context(), w); err != nil {
			return
		} else if blocked {
			writeError(w, http.StatusConflict, "cannot demote the last owner")
			return
		}
	}

	if err := h.store.SetRole(r.Context(), id, role); err != nil {
		h.writeStoreErr(w, err, "updating role")
		return
	}

	user, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "fetching updated user")
		return
	}
	writeJSON(w, http.StatusOK, userResponse{ID: user.ID, Username: user.Username, Role: string(user.Role), CreatedAt: user.CreatedAt})
}

type resetPasswordRequest struct {
	// Password is optional -- omitted, a random one is generated and
	// returned in the response body exactly once, same "shown once,
	// never stored, never recoverable" posture as -seed-admin's initial
	// password (see cmd/api/main.go's runSeedAdmin).
	Password string `json:"password,omitempty"`
}

type resetPasswordResponse struct {
	// Password is only set when the request didn't supply one --
	// omitempty so an admin-supplied reset doesn't echo it back.
	Password string `json:"password,omitempty"`
}

// handleResetPassword is exclusively for an owner/admin acting on
// someone ELSE's account -- it refuses id == the caller's own ID
// outright (POST /auth/password is the only path to changing your own
// password, for anyone, including an owner resetting their own), and
// refuses an owner target unless the caller is themselves an owner
// (RegisterRoutes' RoleAdmin floor already means a caller here is
// admin-or-owner, so "not an owner" in the check below means admin).
func (h *Handler) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	identity, _ := authz.IdentityFromContext(r.Context())
	if id == identity.UserID {
		writeError(w, http.StatusBadRequest, "use POST /auth/password to change your own password")
		return
	}
	target, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "resetting password")
		return
	}
	if target.Role == authz.RoleOwner && !identity.Role.Satisfies(authz.RoleOwner) {
		writeError(w, http.StatusForbidden, "admin cannot reset an owner's password")
		return
	}

	var req resetPasswordRequest
	// An empty body is valid here (generate a random password) --
	// decodeJSON's json.Decode on an empty io.Reader would error, so
	// this endpoint reads the body directly instead of reusing
	// decodeJSON, tolerating "no body at all" as "use defaults."
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	plaintext := req.Password
	generated := false
	if plaintext == "" {
		raw, _, err := newOpaqueToken()
		if err != nil {
			h.logger.Error("generating random password", "error", err)
			writeError(w, http.StatusInternalServerError, "resetting password failed")
			return
		}
		plaintext = raw
		generated = true
	} else if len(plaintext) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := HashPassword(plaintext)
	if err != nil {
		h.logger.Error("hashing password", "error", err)
		writeError(w, http.StatusInternalServerError, "resetting password failed")
		return
	}
	if err := h.store.SetPasswordHash(r.Context(), id, hash); err != nil {
		h.writeStoreErr(w, err, "resetting password")
		return
	}

	resp := resetPasswordResponse{}
	if generated {
		resp.Password = plaintext
	}
	writeJSON(w, http.StatusOK, resp)
}

type changeOwnPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangeOwnPassword is the only path to changing your own
// password, for every role including owner -- distinct from
// handleResetPassword (which acts on someone ELSE's account and never
// asks for a password it could never know) by requiring
// current_password, verified against the caller's own stored hash.
// Like every other password change in this package, it revokes the
// caller's own existing sessions too (SetPasswordHash), including the
// one this very request is authenticated with -- the caller must sign
// in again afterward.
func (h *Handler) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var req changeOwnPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	identity, _ := authz.IdentityFromContext(r.Context())
	hash, err := h.store.GetPasswordHashByID(r.Context(), identity.UserID)
	if err != nil {
		h.writeStoreErr(w, err, "changing password")
		return
	}
	if !ComparePassword(hash, req.CurrentPassword) {
		writeError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}

	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		h.logger.Error("hashing password", "error", err)
		writeError(w, http.StatusInternalServerError, "changing password failed")
		return
	}
	if err := h.store.SetPasswordHash(r.Context(), identity.UserID, newHash); err != nil {
		h.writeStoreErr(w, err, "changing password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type setTimezoneRequest struct {
	Timezone string `json:"timezone"`
}

// maxTimezoneLen bounds the input before it reaches LoadLocation, which
// takes the value as a filesystem-ish lookup key. The longest real IANA
// name is well under this ("America/Argentina/ComodRivadavia", 31).
const maxTimezoneLen = 64

// handleSetTimezone stores the caller's own display-timezone preference
// -- see metadata/migrations/0042_add_user_display_timezone.sql for why
// this is presentation-only and can never affect what data a query
// returns.
//
// Validation is time.LoadLocation against the tzdata embedded in this
// binary (see the time/tzdata import in cmd/api/main.go), not a
// hand-maintained allowlist: the set of valid zone names is the tz
// database's to define, and it changes a few times a year. Rejecting
// unknown names here matters because the value is echoed back to every
// client on the session response -- an unvalidated string would just be
// a stored round trip for whatever someone put in.
func (h *Handler) handleSetTimezone(w http.ResponseWriter, r *http.Request) {
	var req setTimezoneRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Timezone == "" {
		writeError(w, http.StatusBadRequest, "timezone must not be empty")
		return
	}
	if len(req.Timezone) > maxTimezoneLen {
		writeError(w, http.StatusBadRequest, "timezone is not a valid IANA zone name")
		return
	}
	// "Local" is a valid LoadLocation argument but means "whatever zone
	// the *server* process is in", which is meaningless as a per-user
	// display preference and would render differently depending on which
	// host answered. The browser's own zone is the client's business to
	// resolve into a real name before sending it.
	if req.Timezone == "Local" {
		writeError(w, http.StatusBadRequest, "timezone must be a specific IANA zone name, not \"Local\"")
		return
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		writeError(w, http.StatusBadRequest, "timezone is not a valid IANA zone name")
		return
	}

	identity, _ := authz.IdentityFromContext(r.Context())
	if err := h.store.SetDisplayTimezone(r.Context(), identity.UserID, req.Timezone); err != nil {
		h.writeStoreErr(w, err, "setting timezone")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setCookie(w http.ResponseWriter, raw string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    raw,
		Domain:   h.cookies.Domain,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   h.cookies.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Domain:   h.cookies.Domain,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookies.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.logger.Error(action, "error", err)
	writeError(w, http.StatusInternalServerError, action+" failed")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
