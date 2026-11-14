package mailmon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMail is a mail server just big enough for a round: SMTP submission
// and IMAP without TLS, and the sign-in and JMAP calls the watcher makes.
type fakeMail struct {
	t        *testing.T
	user, pw string
	drop     bool // swallow messages instead of delivering them

	mu      sync.Mutex
	inbox   map[string]string // id → subject
	nextID  int
	queue   []time.Time
	tokens  map[string]bool
	smtp    net.Listener
	imap    net.Listener
	http    *httptest.Server
	deleted int
}

func newFakeMail(t *testing.T) *fakeMail {
	f := &fakeMail{t: t, user: "monitor@example.test", pw: "right horse", inbox: map[string]string{}, tokens: map[string]bool{}}
	var err error
	if f.smtp, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if f.imap, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go f.serve(f.smtp, f.smtpConn)
	go f.serve(f.imap, f.imapConn)
	f.http = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() { f.smtp.Close(); f.imap.Close(); f.http.Close() })
	return f
}

func (f *fakeMail) serve(l net.Listener, h func(net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); h(c) }()
	}
}

func (f *fakeMail) smtpConn(c net.Conn) {
	r := bufio.NewReader(c)
	say := func(s string) { c.Write([]byte(s + "\r\n")) }
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
			b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.Fields(line)[2]))
			parts := strings.Split(string(b), "\x00")
			if len(parts) == 3 && parts[1] == f.user && parts[2] == f.pw {
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
			if !f.drop {
				go func() { // delivery takes a moment
					time.Sleep(300 * time.Millisecond)
					f.mu.Lock()
					f.nextID++
					f.inbox[string(rune('a'+f.nextID))] = subj
					f.mu.Unlock()
				}()
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

func (f *fakeMail) imapConn(c net.Conn) {
	r := bufio.NewReader(c)
	c.Write([]byte("* OK fake IMAP ready\r\n"))
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case strings.HasPrefix(rest, "LOGIN "):
			if rest == "LOGIN "+quote(f.user)+" "+quote(f.pw) {
				c.Write([]byte("* CAPABILITY IMAP4rev1\r\n" + tag + " OK logged in\r\n"))
			} else {
				c.Write([]byte(tag + " NO [AUTHENTICATIONFAILED] Authentication failed\r\n"))
			}
		case rest == "LOGOUT":
			c.Write([]byte("* BYE\r\n" + tag + " OK\r\n"))
			return
		}
	}
}

func (f *fakeMail) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/auth":
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["accountName"] != f.user || req["accountSecret"] != f.pw || req["codeChallengeMethod"] != "S256" {
			json.NewEncoder(w).Encode(map[string]string{"type": "failure"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"type": "authenticated", "client_code": "code1"})
	case "/auth/token":
		r.ParseForm()
		if r.Form.Get("code") != "code1" || r.Form.Get("code_verifier") == "" {
			http.Error(w, "bad code", 400)
			return
		}
		f.tokens["tok"] = true
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	case "/jmap/session":
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/jmap/", "capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}},
			"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "m1", "urn:inbuxa:jmap": "m1"}})
	case "/jmap/":
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out [][]any
		var lastIDs []string
		for _, mc := range req.MethodCalls {
			var name, id string
			json.Unmarshal(mc[0], &name)
			json.Unmarshal(mc[2], &id)
			var args map[string]any
			json.Unmarshal(mc[1], &args)
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
				for _, d := range args["destroy"].([]any) {
					delete(f.inbox, d.(string))
					f.deleted++
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
		json.NewEncoder(w).Encode(map[string]any{"methodResponses": out})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeMail) watcher(t *testing.T, pw string) *Watcher {
	p := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(p, []byte(pw+"\n"), 0o600)
	w, err := New(Config{Account: f.user, PasswordFile: p, Server: "127.0.0.1",
		SMTP: f.smtp.Addr().String(), IMAP: f.imap.Addr().String(), URL: f.http.URL,
		ClientID: "cairn-obs", RedirectURL: "https://obs.example.test/cb",
		Every: 5 * time.Second, Wait: 2 * time.Second, PlainIMAP: true, PlainSMTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRoundTrip(t *testing.T) {
	f := newFakeMail(t)
	old := time.Now().Add(-3 * time.Hour)
	f.queue = []time.Time{time.Now().Add(-time.Minute), old}
	w := f.watcher(t, f.pw)
	w.Round(context.Background())
	s := w.Snapshot()
	if s.Error != "" {
		t.Fatal(s.Error)
	}
	for _, si := range s.SignIns {
		if !si.OK {
			t.Errorf("%s sign-in failed: %s", si.Name, si.Error)
		}
	}
	if !s.Trip.OK || s.Trip.Seconds < 0.2 || s.Trip.Seconds > 2 {
		t.Errorf("trip = %+v, want arrived after about 0.3 s", s.Trip)
	}
	if f.deleted != 1 || len(f.inbox) != 0 {
		t.Errorf("deleted %d, inbox %v: the test message should be gone", f.deleted, f.inbox)
	}
	if !s.Queue.Read || s.Queue.Total != 2 || s.Queue.Oldest == nil || !s.Queue.Oldest.Truncate(time.Second).Equal(old.Truncate(time.Second)) {
		t.Errorf("queue = %+v, want 2 with the oldest from %s", s.Queue, old)
	}
	if len(s.History) != 1 || s.History[0].Seconds != s.Trip.Seconds {
		t.Errorf("history = %+v", s.History)
	}
}

func TestWrongPassword(t *testing.T) {
	f := newFakeMail(t)
	w := f.watcher(t, "wrong horse")
	w.Round(context.Background())
	w.Round(context.Background())
	s := w.Snapshot()
	for _, si := range s.SignIns {
		if si.OK || si.Fails != 2 || !strings.Contains(si.Error, "refused") {
			t.Errorf("%s = %+v, want refused twice in a row", si.Name, si)
		}
		if strings.Contains(si.Error, "wrong horse") {
			t.Errorf("%s error shows the password: %s", si.Name, si.Error)
		}
	}
	if s.Trip.OK || s.Trip.Fails != 2 || s.Queue.Read || s.Queue.Fails != 2 {
		t.Errorf("trip %+v queue %+v, want both failing twice", s.Trip, s.Queue)
	}
	if len(s.History) != 2 || s.History[1].Seconds != -1 {
		t.Errorf("history = %+v, want two misses", s.History)
	}
}

func TestMessageNeverArrives(t *testing.T) {
	f := newFakeMail(t)
	f.drop = true
	w := f.watcher(t, f.pw)
	start := time.Now()
	w.Round(context.Background())
	s := w.Snapshot()
	if s.Trip.OK || !strings.Contains(s.Trip.Error, "hadn't arrived after 2s") {
		t.Errorf("trip = %+v", s.Trip)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("round took %s; it should give up after wait (2s)", d)
	}
	for _, si := range s.SignIns {
		if !si.OK {
			t.Errorf("%s sign-in failed: %s", si.Name, si.Error)
		}
	}
	// Once it works again the count starts over.
	f.drop = false
	w.Round(context.Background())
	if s := w.Snapshot(); !s.Trip.OK || s.Trip.Fails != 0 {
		t.Errorf("after recovery trip = %+v", s.Trip)
	}
}

func TestMissingPassword(t *testing.T) {
	if _, err := New(Config{Account: "a@b", PasswordFile: "/nonexistent", Server: "x", ClientID: "c", RedirectURL: "https://x/cb"}); err == nil {
		t.Fatal("want an error for a missing password file")
	}
	if _, err := New(Config{Account: "a@b", PasswordFile: "/etc/hostname", Server: "x"}); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("err = %v, want client_id required", err)
	}
}
