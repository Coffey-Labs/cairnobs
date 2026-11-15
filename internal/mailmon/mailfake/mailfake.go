// Package mailfake is a mail server just big enough for the mail monitor's
// round (internal/mailmon): SMTP submission and IMAP without TLS, and the
// sign-in and JMAP calls it makes. The monitor's tests use it, and so does
// the fleet simulator's demo (scripts/fleet), through mail's stand_in.
// Loopback only.
package mailfake

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server is one fake mail server.
type Server struct {
	User, Password string

	mu      sync.Mutex
	delay   time.Duration     // how long delivery takes
	drop    bool              // swallow messages instead of delivering them
	inbox   map[string]string // id → subject
	nextID  int
	queue   []time.Time
	deleted int
	ls      []net.Listener
	http    *http.Server

	SMTPAddr, IMAPAddr string // host:port
	URL                string // JMAP and sign-in: http://host:port
}

// Start listens on three loopback addresses ("127.0.0.1:0" picks free
// ports) and serves until Close.
func Start(user, password, smtpAddr, imapAddr, httpAddr string) (*Server, error) {
	f := &Server{User: user, Password: password, inbox: map[string]string{}, delay: 300 * time.Millisecond}
	var ls [3]net.Listener
	for i, a := range []string{smtpAddr, imapAddr, httpAddr} {
		l, err := net.Listen("tcp", a)
		if err != nil {
			for _, o := range ls[:i] {
				o.Close()
			}
			return nil, err
		}
		if !l.Addr().(*net.TCPAddr).IP.IsLoopback() {
			l.Close()
			for _, o := range ls[:i] {
				o.Close()
			}
			return nil, fmt.Errorf("mailfake: %s isn't loopback", a)
		}
		ls[i] = l
	}
	f.ls = ls[:]
	f.SMTPAddr, f.IMAPAddr, f.URL = ls[0].Addr().String(), ls[1].Addr().String(), "http://"+ls[2].Addr().String()
	go f.serve(ls[0], f.smtpConn)
	go f.serve(ls[1], f.imapConn)
	f.http = &http.Server{Handler: http.HandlerFunc(f.handle), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.http.Serve(ls[2]) }()
	return f, nil
}

// Close stops all three.
func (f *Server) Close() {
	f.ls[0].Close()
	f.ls[1].Close()
	f.http.Close()
}

// SetDrop makes messages vanish (true) or arrive again (false).
func (f *Server) SetDrop(drop bool) {
	f.mu.Lock()
	f.drop = drop
	f.mu.Unlock()
}

// SetDelay sets how long delivery takes (300 ms to begin with).
func (f *Server) SetDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

// SetQueue sets the outgoing queue: one creation time per message.
func (f *Server) SetQueue(q []time.Time) {
	f.mu.Lock()
	f.queue = q
	f.mu.Unlock()
}

// Deleted is how many messages were deleted; Inbox how many are left.
func (f *Server) Deleted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted
}

func (f *Server) Inbox() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inbox)
}

func (f *Server) serve(l net.Listener, h func(net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Minute))
			h(c)
		}()
	}
}

func (f *Server) smtpConn(c net.Conn) {
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch cmd {
		case "EHLO":
			say("250-fake")
			say("250 AUTH PLAIN")
		case "AUTH":
			fs := strings.Fields(line)
			if len(fs) < 3 {
				say("501 syntax")
				continue
			}
			b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(fs[2]))
			parts := strings.Split(string(b), "\x00")
			if len(parts) == 3 && parts[1] == f.User && parts[2] == f.Password {
				authed = true
				say("235 ok")
			} else {
				say("535 5.7.8 Authentication credentials invalid")
			}
		case "MAIL", "RCPT":
			if !authed {
				say("530 auth first")
				continue
			}
			say("250 ok")
		case "DATA":
			say("354 go")
			subj := ""
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				if s, ok := strings.CutPrefix(l, "Subject: "); ok {
					subj = strings.TrimSpace(s)
				}
			}
			f.mu.Lock()
			drop, delay := f.drop, f.delay
			f.mu.Unlock()
			if !drop {
				time.AfterFunc(delay, func() { // delivery takes a moment
					f.mu.Lock()
					f.nextID++
					f.inbox[fmt.Sprintf("m%d", f.nextID)] = subj
					f.mu.Unlock()
				})
			}
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (f *Server) imapConn(c net.Conn) {
	r := bufio.NewReader(c)
	_, _ = c.Write([]byte("* OK fake IMAP ready\r\n"))
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case strings.HasPrefix(rest, "LOGIN "):
			if rest == "LOGIN "+quote(f.User)+" "+quote(f.Password) {
				_, _ = c.Write([]byte("* CAPABILITY IMAP4rev1\r\n" + tag + " OK logged in\r\n"))
			} else {
				_, _ = c.Write([]byte(tag + " NO [AUTHENTICATIONFAILED] Authentication failed\r\n"))
			}
		case rest == "LOGOUT":
			_, _ = c.Write([]byte("* BYE\r\n" + tag + " OK\r\n"))
			return
		}
	}
}

func (f *Server) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	switch r.URL.Path {
	case "/api/auth":
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["accountName"] != f.User || req["accountSecret"] != f.Password || req["codeChallengeMethod"] != "S256" {
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "failure"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"type": "authenticated", "client_code": "code1"})
	case "/auth/token":
		_ = r.ParseForm()
		if r.Form.Get("code") != "code1" || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	case "/jmap/session":
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/jmap/", "capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}},
			"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "m1", "urn:inbuxa:jmap": "m1"}})
	case "/jmap/":
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var out [][]any
		var lastIDs []string
		for _, mc := range req.MethodCalls {
			if len(mc) < 3 {
				continue
			}
			var name, id string
			_ = json.Unmarshal(mc[0], &name)
			_ = json.Unmarshal(mc[2], &id)
			var args map[string]any
			_ = json.Unmarshal(mc[1], &args)
			switch name {
			case "Email/query":
				lastIDs = nil
				for k := range f.inbox {
					lastIDs = append(lastIDs, k)
				}
				out = append(out, []any{name, map[string]any{"ids": lastIDs}, id})
			case "Email/get":
				var list []map[string]string
				for _, k := range lastIDs {
					list = append(list, map[string]string{"id": k, "subject": f.inbox[k]})
				}
				out = append(out, []any{name, map[string]any{"list": list}, id})
			case "Email/set":
				ds, _ := args["destroy"].([]any)
				for _, d := range ds {
					if k, ok := d.(string); ok {
						if _, there := f.inbox[k]; there {
							delete(f.inbox, k)
							f.deleted++
						}
					}
				}
				out = append(out, []any{name, map[string]any{}, id})
			case "x:QueuedMessage/query":
				out = append(out, []any{name, map[string]any{"ids": []string{}, "total": len(f.queue)}, id})
			case "x:QueuedMessage/get":
				var list []map[string]any
				for _, t := range f.queue {
					list = append(list, map[string]any{"createdAt": t.UTC().Format(time.RFC3339)})
				}
				out = append(out, []any{name, map[string]any{"list": list}, id})
			default:
				out = append(out, []any{"error", map[string]string{"type": "unknownMethod"}, id})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": out})
	default:
		http.NotFound(w, r)
	}
}
