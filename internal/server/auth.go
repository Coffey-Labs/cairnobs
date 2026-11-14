package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	sessionCookie = "beacon_session"
	sessionTTL    = 12 * time.Hour
)

// session is a signed-in browser: someone a sign-in provider vouched for,
// or (loopback only) the development sign-in.
type session struct {
	User     string
	Email    string
	Role     string
	Provider string // the provider's id; empty for the dev sign-in
	Dev      bool
	Expires  time.Time

	// Provider sessions only: the last good check, and what the provider
	// needs to check again (its tokens). Never sent to the browser.
	Checked time.Time
	ident   *Identity
}

// sessions are kept in memory, keyed by the SHA-256 of the cookie, and
// written through to the database when there is one, so a restart doesn't
// sign anyone out. Neither place holds a usable cookie.
type sessions struct {
	mu    sync.Mutex
	m     map[string]session
	db    *state.DB
	ended func(id string) // tells other copies a session ended (shared.go)
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func cookieID(tok string) string { return HashToken(tok) }

// sealedIdentity is what a provider needs to re-check a session, as stored.
type sealedIdentity struct {
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	Role    string    `json:"role"`
	UID     string    `json:"uid,omitempty"`
	Subject string    `json:"sub,omitempty"`
	Access  string    `json:"access,omitempty"`
	Refresh string    `json:"refresh,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

func (s *sessions) save(id string, v session) {
	if s.db == nil {
		return
	}
	si := sealedIdentity{}
	if v.ident != nil {
		si = sealedIdentity{Name: v.ident.Name, Email: v.ident.Email, Role: v.ident.Role, UID: v.ident.UID, Subject: v.ident.Subject}
		if t := v.ident.tokens; t != nil {
			si.Access, si.Refresh, si.Expires = t.AccessToken, t.RefreshToken, t.expires
		}
	}
	b, _ := json.Marshal(si)
	err := s.db.PutSession(state.Session{TokenSHA256: id, User: v.User, Email: v.Email, Role: v.Role, Provider: v.Provider,
		Dev: v.Dev, Expires: v.Expires, Checked: v.Checked, Sealed: s.db.Seal(b)})
	if err != nil {
		log.Printf("sessions: can't store: %v", err)
	}
}

// load brings back the sessions stored before a restart.
func (s *sessions) load(now time.Time) {
	if s.db == nil {
		return
	}
	stored, err := s.db.Sessions(now)
	if err != nil {
		log.Printf("sessions: can't load: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]session{}
	}
	for _, st := range stored {
		if v, ok := s.fromStored(st); ok {
			s.m[st.TokenSHA256] = v
		}
	}
	if len(s.m) > 0 {
		log.Printf("sessions: %d brought back from the database", len(s.m))
	}
}

// fromStored turns a stored session back into one; one that can't be
// unsealed is deleted.
func (s *sessions) fromStored(st state.Session) (session, bool) {
	v := session{User: st.User, Email: st.Email, Role: st.Role, Provider: st.Provider, Dev: st.Dev, Expires: st.Expires, Checked: st.Checked}
	b, err := s.db.Unseal(st.Sealed)
	if err != nil {
		log.Printf("sessions: one stored session can't be unsealed (state key changed?); dropping it")
		_ = s.db.DeleteSession(st.TokenSHA256)
		return session{}, false
	}
	var si sealedIdentity
	if json.Unmarshal(b, &si) == nil && st.Provider != "" {
		v.ident = &Identity{Name: si.Name, Email: si.Email, Role: si.Role, UID: si.UID, Subject: si.Subject}
		if si.Access != "" {
			v.ident.tokens = &tokens{AccessToken: si.Access, RefreshToken: si.Refresh, expires: si.Expires}
		}
	}
	return v, true
}

// forget drops a session another copy ended, from memory only.
func (s *sessions) forget(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// keepOnly drops every session in memory that's no longer stored.
func (s *sessions) keepOnly(stored map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.m {
		if !stored[id] {
			delete(s.m, id)
		}
	}
}

func (s *sessions) put(w http.ResponseWriter, r *http.Request, v session) {
	tok := newToken()
	id := cookieID(tok)
	now := time.Now()
	v.Expires = now.Add(sessionTTL)
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]session{}
	}
	for k, old := range s.m { // drop expired sessions while we're here
		if now.After(old.Expires) {
			delete(s.m, k)
			if s.db != nil {
				_ = s.db.DeleteSession(k)
			}
		}
	}
	s.m[id] = v
	s.mu.Unlock()
	s.save(id, v)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL / time.Second),
	})
}

func (s *sessions) create(w http.ResponseWriter, r *http.Request, user string, dev bool) {
	s.put(w, r, session{User: user, Role: RoleAdmin, Dev: dev})
}

// get returns the session's id (the cookie's SHA-256) and the session.
func (s *sessions) get(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", session{}, false
	}
	id := cookieID(c.Value)
	s.mu.Lock()
	v, ok := s.m[id]
	expired := ok && time.Now().After(v.Expires)
	s.mu.Unlock()
	if !ok && s.db != nil && s.db.Shared() {
		// Started on another copy: it's in the database.
		if st, err := s.db.Session(id, time.Now()); err == nil {
			if v, ok = s.fromStored(st); ok {
				s.mu.Lock()
				if s.m == nil {
					s.m = map[string]session{}
				}
				s.m[id] = v
				s.mu.Unlock()
			}
		}
	}
	if !ok || expired {
		if expired {
			s.drop(id)
		}
		return "", session{}, false
	}
	return id, v, true
}

func (s *sessions) update(id string, f func(*session)) {
	s.mu.Lock()
	v, ok := s.m[id]
	if ok {
		f(&v)
		s.m[id] = v
	}
	s.mu.Unlock()
	if ok {
		s.save(id, v)
	}
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
	if s.db != nil {
		if err := s.db.DeleteSession(id); err != nil {
			log.Printf("sessions: can't delete: %v", err)
		}
	}
	if s.ended != nil {
		s.ended(id)
	}
}

func (s *sessions) end(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.drop(cookieID(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// agentFor returns the agent a report or checks request comes from: by its
// client certificate on a TLS agent listener (where tokens only enroll), by
// its token otherwise.
func (srv *Server) agentFor(r *http.Request) (string, bool) {
	if srv.atls != nil {
		return srv.agentFromCert(r)
	}
	return srv.agentByToken(r)
}

// agentByToken returns the agent whose token the request carries. Tokens are
// compared by SHA-256 in constant time; the plain token is never stored.
func (srv *Server) agentByToken(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	got := HashToken(tok)
	name, found := "", false
	for _, a := range srv.cfg.Agents {
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.TokenSHA256)) == 1 {
			name, found = a.Name, true
		}
	}
	return name, found
}

// HashToken is what goes in the config for a token.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewAgentToken makes a token for a new agent.
func NewAgentToken() string { return newToken() }

func readSecret(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
