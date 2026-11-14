package server

// Passwords with TOTP for the local directory, and the
// first admin on a new install.
//
// Passkeys need HTTPS (or localhost), which a new install on a LAN address
// doesn't have yet. So a local account can instead set a password together
// with an authenticator app's codes: both are needed to sign in, never the
// password alone. Passwords are kept as Argon2id hashes, TOTP secrets
// sealed with the state key, and each code works once. Failed sign-ins are
// limited per address and per account.
//
// The first admin: while there are no accounts and no other way in,
// obs-beacond logs a one-time link on every start. Whoever opens it (and so
// can read the server's log) names the first admin and sets up their
// sign-in; the link then stops working.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/totp"
)

const noPasskeys = "Passkeys need Cairn OBS on HTTPS (or localhost). Use a password with an authenticator app for now, or set alerts.public_url to its https:// address."

// Argon2id parameters (RFC 9106's second recommendation, two lanes).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
)

// argonGate bounds how many hashes run at once: each takes 64 MiB.
var argonGate = make(chan struct{}, 2)

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	argonGate <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-argonGate
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// checkPassword compares pw with a hash from hashPassword. An empty hash
// still does the work, so an unknown account takes as long as a known one.
func checkPassword(hash, pw string) bool {
	var v, m, t, p int
	var saltB64, keyB64 string
	parts := strings.Split(hash, "$")
	ok := len(parts) == 6 && parts[1] == "argon2id"
	if ok {
		_, err1 := fmt.Sscanf(parts[2], "v=%d", &v)
		_, err2 := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p)
		saltB64, keyB64 = parts[4], parts[5]
		ok = err1 == nil && err2 == nil && v == argon2.Version && m > 0 && m <= 1<<20 && t > 0 && t <= 16 && p > 0 && p <= 16
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(saltB64)
	want, err2 := base64.RawStdEncoding.DecodeString(keyB64)
	if !ok || err1 != nil || err2 != nil || len(want) == 0 {
		m, t, p, salt, want = argonMemory, argonTime, argonThreads, make([]byte, 16), make([]byte, argonKeyLen)
		ok = false
	}
	argonGate <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	<-argonGate
	return subtle.ConstantTimeCompare(got, want) == 1 && ok
}

// passwordProblem says what's wrong with a new password; "" when nothing.
func passwordProblem(pw, email string) string {
	switch n := len([]rune(pw)); {
	case n < 12:
		return "Use at least 12 characters."
	case len(pw) > 256:
		return "That's too long (256 at most)."
	case strings.EqualFold(pw, email):
		return "Don't use your email address."
	}
	return ""
}

// --- limits on failed sign-ins ---

const (
	failWindow = 15 * time.Minute
	maxFails   = 5 // per account and per address, in failWindow
)

// signinLimiter counts failures in the database, so the limit holds however
// many copies of obs-beacond share the work.
type signinLimiter struct{ db *state.DB }

func newSigninLimiter(db *state.DB) *signinLimiter { return &signinLimiter{db: db} }

// blocked says whether any of keys has failed too often lately. If the
// database can't say, it's blocked: better a wait than no limit.
func (s *signinLimiter) blocked(now time.Time, keys ...string) bool {
	n, err := s.db.SigninFailures(now.Add(-failWindow), keys...)
	if err != nil {
		log.Printf("sign-in limits: %v", err)
		return true
	}
	for _, k := range keys {
		if n[k] >= maxFails {
			return true
		}
	}
	return false
}

func (s *signinLimiter) fail(now time.Time, keys ...string) {
	if err := s.db.AddSigninFailure(now, now.Add(-failWindow), keys...); err != nil {
		log.Printf("sign-in limits: %v", err)
	}
}

func (s *signinLimiter) clear(key string) {
	if err := s.db.ClearSigninFailures(key); err != nil {
		log.Printf("sign-in limits: %v", err)
	}
}

// clientIP is the address a request came from: the proxy's word for it
// only when the proxy is on this machine.
func clientIP(r *http.Request) string {
	host := hostOnly(r.RemoteAddr)
	if loopbackHost(host) {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			parts := strings.Split(f, ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

// --- setting a password and TOTP from a setup link ---

// ceremonyTOTP is the kind of ceremony for a TOTP secret being set up,
// kept by its setup link's SHA-256.
const ceremonyTOTP = "totp-setup"

const totpSetupTTL = 15 * time.Minute

// setupForPassword checks a setup link for the password route.
func (srv *Server) setupForPassword(w http.ResponseWriter, r *http.Request, token string) (state.User, string, bool) {
	l := srv.local
	if l == nil || !l.passwords || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return state.User{}, "", false
	}
	u, sum, err := srv.setupUser(token)
	if err != nil || u.Disabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This setup link has expired or was already used. Ask an admin for a new one."})
		return state.User{}, "", false
	}
	if u.Emergency {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Emergency accounts use a passkey only."})
		return state.User{}, "", false
	}
	if err := rightDoor(u, r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return state.User{}, "", false
	}
	return u, sum, true
}

// handleSetupTOTP makes the TOTP secret for the authenticator app. It is
// kept here, not stored, until a code proves the app has it.
func (srv *Server) handleSetupTOTP(w http.ResponseWriter, r *http.Request) {
	var req setupReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	u, sum, ok := srv.setupForPassword(w, r, req.Token)
	if !ok {
		return
	}
	secret := totp.NewSecret()
	if err := srv.db.PutCeremony(ceremonyTOTP, sum, []byte(secret), time.Now().Add(totpSetupTTL)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": totp.URI("Cairn OBS", u.Email, secret)})
}

// handleSetupPassword sets the password once a code shows the app has the
// secret, uses up the link and signs in.
func (srv *Server) handleSetupPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	u, sum, ok := srv.setupForPassword(w, r, req.Token)
	if !ok {
		return
	}
	if p := passwordProblem(req.Password, u.Email); p != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": p})
		return
	}
	pend, err := srv.db.Ceremony(ceremonyTOTP, sum, time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That took too long: start the authenticator step again."})
		return
	}
	step, ok := totp.Verify(string(pend), req.Code, time.Now(), 0)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That code doesn't match. Check the app shows Cairn OBS, and the phone's clock is right."})
		return
	}
	if err := srv.db.SetPassword(u.ID, state.PasswordLogin{Hash: hashPassword(req.Password), TOTP: srv.db.Seal(pend), LastStep: step}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = srv.db.DropCeremony(ceremonyTOTP, sum)
	_ = srv.db.UseSetupLink(sum)
	u.LastSignIn = time.Now()
	_ = srv.db.PutUser(u)
	srv.audit(u.Name, "set a password and authenticator", "")
	srv.startLocalSession(w, r, u)
	srv.audit(u.Name, "signed in", "with a password and code as "+u.Role)
	writeJSON(w, http.StatusOK, map[string]string{"user": u.Name, "role": u.Role})
}

// --- signing in with a password and code ---

const signinRefused = "That email, password and code don't match an account."

func (srv *Server) handlePasswordSignin(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || !l.passwords || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	now := time.Now()
	email := strings.ToLower(strings.TrimSpace(req.Email))
	acct, ip := "acct:"+email, "ip:"+clientIP(r)
	if l.limit.blocked(now, acct, ip) {
		srv.audit("someone", "was refused", "password sign-in for "+email+": too many tries")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many tries. Wait 15 minutes and try again."})
		return
	}
	u, err := srv.db.UserByEmail(email)
	var login state.PasswordLogin
	if err == nil {
		login, err = srv.db.Password(u.ID)
	}
	// Always hash, so an unknown account takes as long as a wrong password.
	pwOK := checkPassword(login.Hash, req.Password)
	if err != nil || !pwOK {
		l.limit.fail(now, acct, ip)
		srv.audit("someone", "was refused", "password sign-in for "+email+": wrong email or password")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": signinRefused})
		return
	}
	secret, err := srv.db.Unseal(login.TOTP)
	step, codeOK := int64(0), false
	if err == nil {
		step, codeOK = totp.Verify(string(secret), req.Code, now, login.LastStep)
	}
	if codeOK {
		codeOK, _ = srv.db.UseTOTPStep(u.ID, step)
	}
	if !codeOK {
		l.limit.fail(now, acct, ip)
		srv.audit(u.Name, "was refused", "password sign-in: wrong or reused code")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": signinRefused})
		return
	}
	if u.Disabled {
		srv.audit(u.Name, "was refused", "password sign-in: the account is disabled")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "This account is disabled."})
		return
	}
	if err := rightDoor(u, r); err != nil {
		srv.audit(u.Name, "was refused", "password sign-in at the wrong door")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	l.limit.clear(acct)
	u.LastSignIn = now
	_ = srv.db.PutUser(u)
	srv.startLocalSession(w, r, u)
	srv.audit(u.Name, "signed in", "with a password and code as "+u.Role)
	writeJSON(w, http.StatusOK, map[string]string{"user": u.Name, "role": u.Role})
}

// --- the first admin ---

const (
	metaFirstAdmin = "first_admin" // the first-admin link: SHA-256 and expiry
	firstAdminTTL  = 24 * time.Hour
	firstPrefix    = "first-"
)

// needsFirstAdmin says whether this install has no way in yet: a local
// directory with no admin who can sign in (an enabled one with a passkey or
// a password), and no other sign-in configured.
func (srv *Server) needsFirstAdmin() bool {
	if srv.local == nil || srv.db == nil || len(srv.providers) > 0 {
		return false
	}
	n, err := srv.db.UsableAdmins()
	return err == nil && n == 0
}

// firstRun logs a new first-admin link when there's no way in yet. Every
// start makes a new one; the old stops working.
func (srv *Server) firstRun(now time.Time) error {
	if !srv.needsFirstAdmin() {
		if srv.db != nil && srv.db.Meta(metaFirstAdmin) != "" {
			return srv.db.SetMeta(metaFirstAdmin, "")
		}
		return nil
	}
	tok := newToken()
	if err := srv.db.SetMeta(metaFirstAdmin, fmt.Sprintf("%s %d", HashToken(tok), now.Add(firstAdminTTL).Unix())); err != nil {
		return err
	}
	log.Printf("No accounts yet. To make the first admin, open (once, within 24 hours; a restart makes a new link):\n\n    %s/setup#%s%s\n", srv.firstAdminBase(), firstPrefix, tok)
	return nil
}

// firstAdminBase is the best guess at the address a browser reaches.
func (srv *Server) firstAdminBase() string {
	if srv.local.origin != "" {
		return srv.local.origin
	}
	host, port, err := net.SplitHostPort(srv.cfg.Listen)
	if err != nil {
		return "http://" + srv.cfg.Listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if h, err := os.Hostname(); err == nil {
			host = h
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (srv *Server) firstAdminLive(token string, now time.Time) bool {
	v := srv.db.Meta(metaFirstAdmin)
	sum, exp, ok := strings.Cut(v, " ")
	if !ok {
		return false
	}
	var until int64
	if _, err := fmt.Sscan(exp, &until); err != nil || now.Unix() >= until {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sum), []byte(HashToken(token))) == 1
}

var firstMu sync.Mutex

// handleFirstAdmin makes the first admin from the first-admin link and
// answers with an ordinary setup link token for them, which the setup page
// carries on with.
func (srv *Server) handleFirstAdmin(w http.ResponseWriter, r *http.Request) {
	if srv.local == nil || srv.db == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Check bool   `json:"check"` // only say whether the link works
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	firstMu.Lock()
	defer firstMu.Unlock()
	now := time.Now()
	token := strings.TrimPrefix(req.Token, firstPrefix)
	if !srv.needsFirstAdmin() || !srv.firstAdminLive(token, now) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This link has expired or was already used. Restarting obs-beacond logs a new one while there are no accounts."})
		return
	}
	if req.Check {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	req.Name, req.Email = strings.TrimSpace(req.Name), strings.ToLower(strings.TrimSpace(req.Email))
	if a, err := mail.ParseAddress(req.Email); err != nil || a.Address != req.Email || req.Name == "" || len(req.Name) > 80 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A name and a plain email address are needed."})
		return
	}
	u := state.User{ID: randomHex(16), Name: req.Name, Email: req.Email, Role: RoleAdmin, Created: now}
	if old, err := srv.db.UserByEmail(req.Email); err == nil {
		// An account that never finished setting up (an earlier try at this,
		// say): it becomes the admin. needsFirstAdmin says none can sign in.
		u.ID, u.Created = old.ID, old.Created
	}
	if err := srv.db.PutUser(u); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = srv.db.SetMeta(metaFirstAdmin, "")
	setup := newToken()
	if err := srv.db.PutSetupLink(HashToken(setup), u.ID, now.Add(time.Hour)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(u.Name, "became the first admin", "with the link from the server's log, from "+clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"setup": setup})
}

// signinEntry is the local directory on the sign-in page.
func (l *localDir) signinEntry() map[string]any {
	return map[string]any{"id": l.id, "label": l.label, "kind": "passkey", "passkeys": l.wa != nil || l.waDoor != nil, "passwords": l.passwords}
}
