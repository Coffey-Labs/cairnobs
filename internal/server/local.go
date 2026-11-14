package server

// The local directory: accounts Cairn OBS keeps itself, for
// people who aren't inbuxa admins (viewers) and for installs without single
// sign-on. Passkeys first: an admin creates the account and hands over a
// one-time setup link; the person registers a passkey with it and signs in
// with that from then on. Where passkeys can't work (no HTTPS yet) and
// `passwords` is on, a password with a TOTP code instead (password.go);
// never a password alone.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/cairnobs/cairnobs/internal/notify"
	"github.com/cairnobs/cairnobs/internal/state"
)

// RoleOperator sees everything an admin does, log lines and setup included,
// and handles alerts (acknowledges them, writes notes), but changes no setup.
const RoleOperator = "operator"

// RoleViewer sees what's being watched but changes nothing; no log lines,
// no setup, no audit log.
const RoleViewer = "viewer"

// validRole says whether a local account may hold the role.
func validRole(r string) bool { return r == RoleAdmin || r == RoleOperator || r == RoleViewer }

const (
	waCookie     = "beacon_passkey" // ties a passkey ceremony to the browser that started it
	setupLinkTTL = 24 * time.Hour
)

// localDir is the local directory's sign-in.
type localDir struct {
	id, label string
	origin    string             // "" when obs-beacond doesn't know its own address
	wa        *webauthn.WebAuthn // nil without an https origin: no passkeys
	db        *state.DB
	passwords bool // password + TOTP allowed
	builtIn   bool // no sign-in providers configured: this one is implied

	limit *signinLimiter

	// The emergency door (emergency_listen): its own origin on localhost,
	// so emergency accounts' passkeys work there and nowhere else.
	doorOrigin string
	waDoor     *webauthn.WebAuthn
}

// waPending is a passkey ceremony under way, kept in the database by the
// ceremony cookie, so it can finish on any copy.
type waPending struct {
	Data   webauthn.SessionData `json:"data"`
	UserID string               `json:"user_id,omitempty"` // setup only
	Token  string               `json:"token,omitempty"`   // setup only: the link's SHA-256
}

const (
	ceremonyPasskey = "passkey"
	passkeyTTL      = 5 * time.Minute
)

func (c *ProviderConfig) checkLocal() error {
	u, err := url.Parse(c.Origin)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) || u.Host == "" || u.Path != "" {
		return fmt.Errorf("sign-in provider %s: origin must be Cairn OBS's own https://host (http only on localhost)", c.ID)
	}
	if c.Label == "" {
		c.Label = "Sign in with a passkey"
	}
	return nil
}

// passkeyOrigin is the origin passkeys can be bound to: https, or http on
// loopback (browsers allow passkeys there). "" otherwise.
func passkeyOrigin(o string) string {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || !(u.Scheme == "https" || (u.Scheme == "http" && loopbackHost(u.Hostname()))) {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func newLocalDir(c ProviderConfig, db *state.DB, emergencyListen string) (*localDir, error) {
	if db == nil {
		return nil, fmt.Errorf("sign-in provider %s: the local directory needs `database` in obs-beacond.yaml", c.ID)
	}
	l := &localDir{id: c.ID, label: c.Label, origin: strings.TrimRight(c.Origin, "/"), db: db,
		passwords: c.Passwords, limit: newSigninLimiter(db)}
	if o := passkeyOrigin(c.Origin); o != "" {
		u, _ := url.Parse(o)
		wa, err := webauthn.New(&webauthn.Config{RPID: u.Hostname(), RPDisplayName: "Cairn OBS", RPOrigins: []string{o}})
		if err != nil {
			return nil, fmt.Errorf("sign-in provider %s: %w", c.ID, err)
		}
		l.wa = wa
	}
	if emergencyListen != "" {
		var err error
		// Reached as http://localhost:PORT through `ssh -L PORT:127.0.0.1:PORT host`.
		_, port, _ := strings.Cut(emergencyListen, "]:")
		if port == "" {
			_, port, _ = strings.Cut(emergencyListen, ":")
		}
		l.doorOrigin = "http://localhost:" + port
		if l.waDoor, err = webauthn.New(&webauthn.Config{RPID: "localhost", RPDisplayName: "Cairn OBS (emergency)", RPOrigins: []string{l.doorOrigin}}); err != nil {
			return nil, fmt.Errorf("emergency door: %w", err)
		}
	}
	return l, nil
}

type doorKey struct{}

// atDoor says whether the request came in through the emergency door.
func atDoor(r *http.Request) bool { return r.Context().Value(doorKey{}) != nil }

// waFor is the WebAuthn setup for the door the request used (nil when the
// emergency door isn't set up).
func (l *localDir) waFor(r *http.Request) *webauthn.WebAuthn {
	if atDoor(r) {
		return l.waDoor
	}
	return l.wa
}

// rightDoor: emergency accounts only through the emergency door, everyone
// else only through the front.
func rightDoor(u state.User, r *http.Request) error {
	switch {
	case u.Emergency && !atDoor(r):
		return errors.New("This is an emergency account: it signs in only through the emergency door (an SSH tunnel to the server's host).")
	case !u.Emergency && atDoor(r):
		return errors.New("Only emergency accounts sign in through the emergency door.")
	}
	return nil
}

// waUser is a local account as the WebAuthn library sees it.
type waUser struct {
	state.User
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte {
	b, _ := hex.DecodeString(u.ID)
	return b
}
func (u waUser) WebAuthnName() string                       { return u.Email }
func (u waUser) WebAuthnDisplayName() string                { return u.Name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (l *localDir) load(id string) (waUser, []state.Passkey, error) {
	u, err := l.db.GetUser(id)
	if err != nil {
		return waUser{}, nil, err
	}
	pks, err := l.db.Passkeys(id)
	if err != nil {
		return waUser{}, nil, err
	}
	wu := waUser{User: u}
	for _, p := range pks {
		var c webauthn.Credential
		if json.Unmarshal([]byte(p.Credential), &c) == nil {
			wu.creds = append(wu.creds, c)
		}
	}
	return wu, pks, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// remember starts a ceremony: its state stays in obs-beacond, the browser gets
// a cookie that names it.
func (l *localDir) remember(w http.ResponseWriter, r *http.Request, p waPending) {
	key := newToken()
	b, _ := json.Marshal(p)
	if err := l.db.PutCeremony(ceremonyPasskey, key, b, time.Now().Add(passkeyTTL)); err != nil {
		log.Printf("passkeys: %v", err) // the ceremony then fails as expired
	}
	http.SetCookie(w, &http.Cookie{Name: waCookie, Value: key, Path: "/auth/", HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: int(passkeyTTL.Seconds())})
}

// take ends a ceremony (one try per ceremony).
func (l *localDir) take(r *http.Request) (waPending, bool) {
	c, err := r.Cookie(waCookie)
	if err != nil {
		return waPending{}, false
	}
	b, err := l.db.TakeCeremony(ceremonyPasskey, c.Value, time.Now())
	var p waPending
	if err != nil || json.Unmarshal(b, &p) != nil {
		return waPending{}, false
	}
	return p, true
}

func needBeaconHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Beacon") != "1" {
		http.Error(w, "missing X-Beacon header", http.StatusForbidden)
		return false
	}
	return true
}

// --- sign in with a passkey ---

func (srv *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || l.waFor(r) == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	opts, data, err := l.waFor(r).BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	l.remember(w, r, waPending{Data: *data})
	writeJSON(w, http.StatusOK, opts)
}

func (srv *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	p, ok := l.take(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That took too long; try again."})
		return
	}
	var found waUser
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		u, _, err := l.load(hex.EncodeToString(userHandle))
		if err != nil {
			return nil, err
		}
		found = u
		return u, nil
	}
	wa := l.waFor(r)
	if wa == nil {
		http.NotFound(w, r)
		return
	}
	cred, err := wa.FinishDiscoverableLogin(handler, p.Data, r)
	if err != nil || found.ID == "" {
		srv.audit("someone", "was refused", "passkey sign-in: the passkey wasn't recognised")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "That passkey isn't one Cairn OBS knows."})
		return
	}
	if found.Disabled {
		srv.audit(found.Name, "was refused", "passkey sign-in: the account is disabled")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "This account is disabled."})
		return
	}
	if err := rightDoor(found.User, r); err != nil {
		srv.audit(found.Name, "was refused", "passkey sign-in at the wrong door")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	// Keep the signature counter and the last use.
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	if pks, err := l.db.Passkeys(found.ID); err == nil {
		for _, pk := range pks {
			if pk.ID == id {
				b, _ := json.Marshal(cred)
				pk.Credential, pk.LastUsed = string(b), time.Now()
				_ = l.db.PutPasskey(pk)
			}
		}
	}
	found.LastSignIn = time.Now()
	_ = l.db.PutUser(found.User)
	srv.startLocalSession(w, r, found.User)
	if atDoor(r) {
		srv.audit(found.Name, "signed in through the emergency door", "with a passkey; every channel was told")
		srv.emergencyUsed(found.Name, "signed in")
	} else {
		srv.audit(found.Name, "signed in", "with a passkey as "+found.Role)
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": found.Name, "role": found.Role})
}

func (srv *Server) startLocalSession(w http.ResponseWriter, r *http.Request, u state.User) {
	id := &Identity{Name: u.Name, Email: u.Email, Role: u.Role, UID: u.ID}
	srv.sessions.put(w, r, session{User: u.Name, Email: u.Email, Role: u.Role, Provider: srv.local.id, Checked: time.Now(), ident: id})
}

// --- set up a passkey from a setup link ---

type setupReq struct {
	Token string `json:"token"`
}

func (srv *Server) setupUser(token string) (state.User, string, error) {
	sum := HashToken(token)
	uid, err := srv.db.SetupLink(sum, time.Now())
	if err != nil {
		return state.User{}, "", err
	}
	u, err := srv.db.GetUser(uid)
	return u, sum, err
}

// handleSetupInfo tells the setup page whose link it is.
func (srv *Server) handleSetupInfo(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	var req setupReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	u, _, err := srv.setupUser(req.Token)
	if err != nil || u.Disabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This setup link has expired or was already used. Ask an admin for a new one."})
		return
	}
	if err := rightDoor(u, r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": u.Name, "email": u.Email, "role": u.Role, "emergency": u.Emergency,
		"passkeys": l.waFor(r) != nil, "passwords": l.passwords && !u.Emergency, "secure": secureRequest(r) || loopbackHost(hostOnly(r.Host))})
}

func (srv *Server) handleSetupBegin(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	var req setupReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	u, sum, err := srv.setupUser(req.Token)
	if err != nil || u.Disabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This setup link has expired or was already used. Ask an admin for a new one."})
		return
	}
	if err := rightDoor(u, r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	wa := l.waFor(r)
	if wa == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": noPasskeys})
		return
	}
	wu, _, err := l.load(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range wu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, data, err := wa.BeginRegistration(wu,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(exclude))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	l.remember(w, r, waPending{Data: *data, UserID: u.ID, Token: sum})
	writeJSON(w, http.StatusOK, opts)
}

func (srv *Server) handleSetupFinish(w http.ResponseWriter, r *http.Request) {
	l := srv.local
	if l == nil || !needBeaconHeader(w, r) {
		http.NotFound(w, r)
		return
	}
	p, ok := l.take(r)
	if !ok || p.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That took too long; try again."})
		return
	}
	// The link must still be live: it could have been replaced or used meanwhile.
	if uid, err := srv.db.SetupLink(p.Token, time.Now()); err != nil || uid != p.UserID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "This setup link has expired or was already used."})
		return
	}
	wu, _, err := l.load(p.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := rightDoor(wu.User, r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	wa := l.waFor(r)
	if wa == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": noPasskeys})
		return
	}
	cred, err := wa.FinishRegistration(wu, p.Data, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The passkey couldn't be saved: " + err.Error()})
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 60 {
		name = "Passkey"
	}
	b, _ := json.Marshal(cred)
	now := time.Now()
	if err := l.db.PutPasskey(state.Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), UserID: wu.ID, Name: name, Credential: string(b), Created: now, LastUsed: now}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = l.db.UseSetupLink(p.Token)
	wu.LastSignIn = now
	_ = l.db.PutUser(wu.User)
	srv.audit(wu.Name, "added a passkey", name)
	srv.startLocalSession(w, r, wu.User)
	if atDoor(r) {
		srv.audit(wu.Name, "signed in through the emergency door", "with a new passkey; every channel was told")
		srv.emergencyUsed(wu.Name, "set up an emergency passkey and signed in")
	} else {
		srv.audit(wu.Name, "signed in", "with a new passkey as "+wu.Role)
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": wu.Name, "role": wu.Role})
}

// recheckLocal: a local session lives only while its account does.
func (srv *Server) recheckLocal(tok string, s session) bool {
	if s.ident == nil || srv.db == nil {
		return false
	}
	u, err := srv.db.GetUser(s.ident.UID)
	if err != nil || u.Disabled {
		srv.audit(s.User, "was signed out", "the local account was disabled or removed")
		return false
	}
	if u.Role != s.Role || u.Name != s.User {
		srv.sessions.update(tok, func(v *session) {
			v.Role, v.User, v.Email = u.Role, u.Name, u.Email
			v.ident.Role, v.ident.Name, v.ident.Email = u.Role, u.Name, u.Email
		})
	}
	return true
}

// --- admin: the Users page ---

// needAdmin is needSession for things only admins may do.
func (srv *Server) needAdmin(next http.HandlerFunc) http.HandlerFunc {
	return srv.needSession(func(w http.ResponseWriter, r *http.Request) {
		if _, s, ok := srv.sessions.get(r); !ok || s.Role != RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only admins can do that."})
			return
		}
		next(w, r)
	})
}

// needOperator is needSession for seeing everything: setup read-only, log
// lines, the audit log, and handling alerts. Admins and operators.
func (srv *Server) needOperator(next http.HandlerFunc) http.HandlerFunc {
	return srv.needSession(func(w http.ResponseWriter, r *http.Request) {
		if !srv.seesAll(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Only admins and operators can see that."})
			return
		}
		next(w, r)
	})
}

// seesAll says whether the request's session is an admin's or an operator's.
func (srv *Server) seesAll(r *http.Request) bool {
	_, s, ok := srv.sessions.get(r)
	return ok && (s.Role == RoleAdmin || s.Role == RoleOperator)
}

func (srv *Server) who(r *http.Request) string {
	if _, s, ok := srv.sessions.get(r); ok {
		return s.User
	}
	return "someone"
}

type userView struct {
	state.User
	Passkeys     []state.Passkey `json:"passkeys"`
	PendingSetup bool            `json:"pending_setup"`
}

func (srv *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if srv.local == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "users": []userView{}})
		return
	}
	us, err := srv.db.Users()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := []userView{}
	for _, u := range us {
		pks, _ := srv.db.Passkeys(u.ID)
		out = append(out, userView{User: u, Passkeys: pks, PendingSetup: srv.db.PendingSetup(u.ID, time.Now())})
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "users": out, "origin": srv.local.origin, "door": srv.local.doorOrigin, "door_listen": srv.cfg.EmergencyListen,
		"passkeys": srv.local.wa != nil, "passwords": srv.local.passwords})
}

func (srv *Server) newSetupLink(u state.User) (string, error) {
	tok := newToken()
	if err := srv.db.PutSetupLink(HashToken(tok), u.ID, time.Now().Add(setupLinkTTL)); err != nil {
		return "", err
	}
	// The token travels in the fragment, so it never reaches a server log.
	// An emergency account's link opens at the emergency door.
	base := srv.local.origin
	if u.Emergency {
		base = srv.local.doorOrigin
	}
	return base + "/setup#" + tok, nil
}

func (srv *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if srv.local == nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Name, Email, Role string
		Emergency         bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	req.Name, req.Email = strings.TrimSpace(req.Name), strings.ToLower(strings.TrimSpace(req.Email))
	if a, err := mail.ParseAddress(req.Email); err != nil || a.Address != req.Email || req.Name == "" || len(req.Name) > 80 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A name and a plain email address are needed."})
		return
	}
	if req.Emergency {
		if srv.local.waDoor == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The emergency door isn't set up (emergency_listen in obs-beacond.yaml)."})
			return
		}
		req.Role = RoleAdmin // an emergency account is an admin by definition
	}
	if !validRole(req.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Role is admin, operator or viewer."})
		return
	}
	u := state.User{ID: randomHex(32), Name: req.Name, Email: req.Email, Role: req.Role, Created: time.Now(), Emergency: req.Emergency}
	if err := srv.db.PutUser(u); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "There's already an account with that email."})
		return
	}
	link, err := srv.newSetupLink(u)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	kind := u.Role
	if u.Emergency {
		kind = "emergency admin"
	}
	srv.audit(srv.who(r), "added a local account", fmt.Sprintf("%s <%s> as %s", u.Name, u.Email, kind))
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "setup_link": link, "expires_in_s": int(setupLinkTTL / time.Second)})
}

func (srv *Server) userFromPath(w http.ResponseWriter, r *http.Request) (state.User, bool) {
	if srv.local == nil {
		http.NotFound(w, r)
		return state.User{}, false
	}
	u, err := srv.db.GetUser(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No such account."})
		return u, false
	}
	return u, true
}

func (srv *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	u, ok := srv.userFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	var changes []string
	if req.Role != nil && *req.Role != u.Role {
		if u.Emergency {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "An emergency account is always an admin."})
			return
		}
		if !validRole(*req.Role) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Role is admin, operator or viewer."})
			return
		}
		changes = append(changes, "role "+u.Role+" → "+*req.Role)
		u.Role = *req.Role
	}
	if req.Disabled != nil && *req.Disabled != u.Disabled {
		u.Disabled = *req.Disabled
		changes = append(changes, map[bool]string{true: "disabled", false: "enabled"}[u.Disabled])
	}
	if err := srv.db.PutUser(u); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(changes) > 0 {
		srv.audit(srv.who(r), "changed a local account", u.Name+": "+strings.Join(changes, ", "))
	}
	writeJSON(w, http.StatusOK, u)
}

func (srv *Server) handleUserSetupLink(w http.ResponseWriter, r *http.Request) {
	u, ok := srv.userFromPath(w, r)
	if !ok {
		return
	}
	link, err := srv.newSetupLink(u)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(srv.who(r), "made a setup link", "for "+u.Name)
	writeJSON(w, http.StatusOK, map[string]any{"setup_link": link, "expires_in_s": int(setupLinkTTL / time.Second)})
}

func (srv *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := srv.userFromPath(w, r)
	if !ok {
		return
	}
	if err := srv.db.DeleteUser(u.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(srv.who(r), "removed a local account", u.Name+" <"+u.Email+">")
	w.WriteHeader(http.StatusNoContent)
}

// handlePasswordDelete removes an account's password and TOTP; it signs in
// with a passkey, or gets a new setup link.
func (srv *Server) handlePasswordDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := srv.userFromPath(w, r)
	if !ok {
		return
	}
	if err := srv.db.ClearPassword(u.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(srv.who(r), "removed a password and authenticator", "from "+u.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := srv.userFromPath(w, r)
	if !ok {
		return
	}
	if err := srv.db.DeletePasskey(u.ID, r.PathValue("pid")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, state.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": "No such passkey."})
		return
	}
	srv.audit(srv.who(r), "removed a passkey", "from "+u.Name)
	w.WriteHeader(http.StatusNoContent)
}

// logsFor leaves log lines out for viewers: they can hold things only
// admins and operators should see.
func (srv *Server) logsFor(r *http.Request, rows []ContainerRow) []ContainerRow {
	if srv.seesAll(r) {
		return rows
	}
	out := make([]ContainerRow, len(rows))
	for i, row := range rows {
		row.Logs = nil
		out[i] = row
	}
	return out
}

// emergencyUsed tells every alert channel, whatever its minimum, that the
// emergency door was used: it should never be used quietly.
func (srv *Server) emergencyUsed(who, what string) {
	srv.alerts.tellEveryone(notify.Notice{Kind: "firing", Severity: "critical", Title: "Emergency sign-in: " + who,
		Detail: who + " " + what + " through Cairn OBS's emergency door (an SSH tunnel to its host). If that wasn't expected, disable the account on the Users page and look at who has SSH access.",
		Since:  time.Now()})
}

// EmergencyHandler is the emergency door: the whole web interface, marked
// so that only emergency accounts sign in here.
func (srv *Server) EmergencyHandler() http.Handler {
	h := srv.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), doorKey{}, true)))
	})
}
