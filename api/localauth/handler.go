package localauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sentry/sentry/api/authz"
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
	DeleteUser(ctx context.Context, id string) error
	SetPasswordHash(ctx context.Context, userID, hash string) error
	CreateSession(ctx context.Context, userID, tenantID string, role authz.Role, ttl time.Duration) (string, error)
	DeleteSessionByHash(ctx context.Context, tokenHash string) error
}

// CookieConfig is the deployment-specific half of how the session
// cookie is set -- everything else about it (name, HttpOnly, SameSite)
// is fixed by this package, not configurable per deployment.
type CookieConfig struct {
	// Domain is typically empty for local dev (host-only cookie, works
	// fine when web/api are both localhost:<port>) and something like
	// ".sentry.example.com" in production, so the same cookie is sent to
	// api.sentry.example.com and alerting.sentry.example.com too -- see
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
// shape. Login/logout/session are deliberately NOT RequireRole-wrapped
// with anything above RoleViewer's floor: login is how you become
// authenticated in the first place, logout/session must work for any
// already-authenticated user regardless of role.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/login", h.handleLogin)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
	mux.HandleFunc("GET /auth/session", authz.RequireRole(h.authorizer, authz.RoleViewer, h.handleGetSession))

	mux.HandleFunc("GET /auth/users", authz.RequireRole(h.authorizer, authz.RoleOwner, h.handleListUsers))
	mux.HandleFunc("POST /auth/users", authz.RequireRole(h.authorizer, authz.RoleOwner, h.handleCreateUser))
	mux.HandleFunc("DELETE /auth/users/{id}", authz.RequireRole(h.authorizer, authz.RoleOwner, h.handleDeleteUser))
	mux.HandleFunc("POST /auth/users/{id}/reset-password", authz.RequireRole(h.authorizer, authz.RoleOwner, h.handleResetPassword))
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sessionResponse struct {
	// Token duplicates what the Set-Cookie header already carries,
	// specifically for non-browser callers with no cookie jar --
	// sentryctl captures this into SENTRYCTL_TOKEN and sends it back as
	// Authorization: Bearer (see authorizer.go's credentialFromRequest,
	// which accepts either). The web UI ignores this field entirely and
	// relies on the cookie.
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
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

// handleDeleteUser deliberately does not stop an admin from deleting
// their own account -- this package has no separate "you can't remove
// the last admin" guard; a single-operator prototype deployment is
// expected to know what it's doing here, same trust level the rest of
// this codebase's admin-only endpoints assume.
func (h *Handler) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		h.writeStoreErr(w, err, "deleting user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (h *Handler) handleResetPassword(w http.ResponseWriter, r *http.Request) {
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
	if err := h.store.SetPasswordHash(r.Context(), r.PathValue("id"), hash); err != nil {
		h.writeStoreErr(w, err, "resetting password")
		return
	}

	resp := resetPasswordResponse{}
	if generated {
		resp.Password = plaintext
	}
	writeJSON(w, http.StatusOK, resp)
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
