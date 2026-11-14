// Package fakeinbuxa is a stand-in for inbuxa's sign-in, for tests and local
// development only. It speaks the parts obs-beacond uses: discovery, the
// authorization-code flow with PKCE, the token endpoint (code and refresh),
// /api/account permissions and userinfo. Like the real server it accepts
// only its one registered client and that client's exact redirect address.
package fakeinbuxa

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Account is someone who can sign in to the fake.
type Account struct {
	Name        string
	Email       string
	Permissions []string
}

// Admin and Tenant are the two accounts the fake starts with: a System
// Administrator (holds the superuser-only permissions) and a tenant admin
// (doesn't).
var (
	Admin  = Account{Name: "Pat Admin", Email: "pat@example.org", Permissions: []string{"authenticate", "impersonate", "liveMetrics", "liveTracing", "sysLegalHoldGet", "sysAccountGet"}}
	Tenant = Account{Name: "Helpdesk", Email: "helpdesk@tenant.example.com", Permissions: []string{"authenticate", "sysAccountGet", "sysTenantGet"}}
)

type grant struct {
	account   string
	challenge string
	redirect  string
}

// Server is the fake. Set Down to make /api/account fail as if inbuxa were unreachable.
type Server struct {
	ClientID    string
	RedirectURL string
	// BareUserinfo makes userinfo answer with no name or email.
	BareUserinfo bool

	mu       sync.Mutex
	accounts map[string]Account
	codes    map[string]grant
	access   map[string]string // token -> account key
	refresh  map[string]string
	down     bool
}

// New makes a fake with the Admin and Tenant accounts.
func New(clientID, redirectURL string) *Server {
	return &Server{
		ClientID: clientID, RedirectURL: redirectURL,
		accounts: map[string]Account{"admin": Admin, "tenant": Tenant},
		codes:    map[string]grant{}, access: map[string]string{}, refresh: map[string]string{},
	}
}

// SetPermissions changes an account's permissions, as removing a role would.
func (s *Server) SetPermissions(key string, perms []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[key]
	a.Permissions = perms
	s.accounts[key] = a
}

// SetDown makes the API answer 503, as if inbuxa were down.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
}

func token() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Approve plays the user's part on the sign-in page: given the authorize URL
// obs-beacond redirected to, it returns where the fake would send the browser.
func (s *Server) Approve(authorizeURL, account string) (string, error) {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return "", err
	}
	return s.approve(u.Query(), account)
}

func (s *Server) approve(q url.Values, account string) (string, error) {
	if q.Get("client_id") != s.ClientID {
		return "", fmt.Errorf("unregistered client %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != s.RedirectURL {
		return "", fmt.Errorf("redirect %q isn't registered", q.Get("redirect_uri"))
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		return "", fmt.Errorf("wants response_type=code with an S256 code challenge")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[account]; !ok {
		return "", fmt.Errorf("no account %q", account)
	}
	code := token()
	s.codes[code] = grant{account: account, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	back := url.Values{"code": {code}, "state": {q.Get("state")}}
	return q.Get("redirect_uri") + "?" + back.Encode(), nil
}

var page = template.Must(template.New("p").Parse(`<!doctype html><meta charset="utf-8"><title>fake inbuxa</title>
<style>body{font:15px system-ui;background:#0e1c26;color:#e7eef2;display:grid;place-items:center;min-height:100vh;margin:0}
form{background:#132633;border:1px solid #1f3847;border-radius:12px;padding:28px;width:360px}
button{display:block;width:100%;margin-top:10px;padding:10px;border-radius:8px;border:1px solid #274455;background:#1a3140;color:inherit;font:inherit;cursor:pointer}
button.pri{background:#46cac3;color:#062a28;border-color:#46cac3} small{color:#93a8b6}</style>
<form method="post"><b>Fake inbuxa</b><br><small>Development stand-in for mail.example.org. Pick who signs in.</small>
{{range $k, $v := .Q}}{{range $v}}<input type="hidden" name="{{$k}}" value="{{.}}">{{end}}{{end}}
<button class="pri" name="account" value="admin">Pat Admin · System Administrator</button>
<button name="account" value="tenant">helpdesk@tenant.example.com · tenant admin</button>
<button name="account" value="cancel">Cancel</button></form>`))

// Handler serves the fake. base is the fake's own URL, for discovery.
func (s *Server) Handler(base string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer":                 base,
			"authorization_endpoint": "/authorize", // relative, as inbuxa sometimes answers
			"token_endpoint":         base + "/token",
			"userinfo_endpoint":      base + "/userinfo",
			"scopes_supported":       []string{"openid", "offline_access"}, // as inbuxa: no email or profile
		})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, map[string]any{"Q": r.URL.Query()})
	})
	mux.HandleFunc("POST /authorize", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("account") == "cancel" {
			back := url.Values{"error": {"access_denied"}, "state": {r.PostForm.Get("state")}}
			http.Redirect(w, r, r.PostForm.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
			return
		}
		to, err := s.approve(r.PostForm, r.PostForm.Get("account"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
	})
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("GET /api/account", func(w http.ResponseWriter, r *http.Request) {
		a, ok, down := s.bearer(r)
		switch {
		case down:
			http.Error(w, "down", http.StatusServiceUnavailable)
		case !ok:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			writeJSON(w, 200, map[string]any{"permissions": a.Permissions, "edition": "enterprise", "locale": "en"})
		}
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		a, ok, down := s.bearer(r)
		if down || !ok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if s.BareUserinfo { // as inbuxa may answer without email/profile scopes
			writeJSON(w, 200, map[string]any{"sub": "e"})
			return
		}
		writeJSON(w, 200, map[string]any{"name": a.Name, "email": a.Email, "preferred_username": a.Email})
	})
	mux.HandleFunc("GET /jmap/session", func(w http.ResponseWriter, r *http.Request) {
		a, ok, down := s.bearer(r)
		if down || !ok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]any{"username": a.Email})
	})
	return mux
}

func (s *Server) bearer(r *http.Request) (Account, bool, bool) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.access[tok]
	return s.accounts[key], ok, s.down
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("client_id") != s.ClientID {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var account string
	switch f.Get("grant_type") {
	case "authorization_code":
		g, ok := s.codes[f.Get("code")]
		delete(s.codes, f.Get("code")) // codes are single-use
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || g.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		account = g.account
	case "refresh_token":
		a, ok := s.refresh[f.Get("refresh_token")]
		if !ok {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		account = a
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	at, rt := token(), token()
	s.access[at], s.refresh[rt] = account, account
	writeJSON(w, 200, map[string]any{"access_token": at, "refresh_token": rt, "expires_in": 3600, "token_type": "Bearer"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
