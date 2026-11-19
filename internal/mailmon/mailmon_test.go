package mailmon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/mailmon/mailfake"
)

// The fake server has no throttle and the timing tests count seconds, so
// the spacing between connections is off and the retry wait short.
func TestMain(m *testing.M) {
	stepGap, retryWait = 0, 50*time.Millisecond
	os.Exit(m.Run())
}

func newFakeMail(t *testing.T) *mailfake.Server {
	f, err := mailfake.Start("monitor@example.test", "right horse", "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

// watcher reads f as the config would: through stand_in.
func watcher(t *testing.T, f *mailfake.Server, pw string) *Watcher {
	p := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(p, []byte(pw+"\n"), 0o600)
	w, err := New(Config{Account: f.User, PasswordFile: p, Server: "127.0.0.1",
		SMTP: f.SMTPAddr, IMAP: f.IMAPAddr, URL: f.URL, StandIn: true,
		ClientID: "cairn-obs", RedirectURL: "https://obs.example.test/cb",
		Every: 5 * time.Second, Wait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRoundTrip(t *testing.T) {
	f := newFakeMail(t)
	old := time.Now().Add(-3 * time.Hour)
	f.SetQueue([]time.Time{time.Now().Add(-time.Minute), old})
	w := watcher(t, f, f.Password)
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
	if f.Deleted() != 1 || f.Inbox() != 0 {
		t.Errorf("deleted %d, %d left: the test message should be gone", f.Deleted(), f.Inbox())
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
	w := watcher(t, f, "wrong horse")
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
	f.SetDrop(true)
	w := watcher(t, f, f.Password)
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
	f.SetDrop(false)
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

func TestStandInOnLoopbackOnly(t *testing.T) {
	base := Config{Account: "a@b", PasswordFile: "/etc/hostname", Server: "127.0.0.1", ClientID: "c", RedirectURL: "https://x/cb", StandIn: true,
		SMTP: "127.0.0.1:2525", IMAP: "127.0.0.1:1143", URL: "http://127.0.0.1:8080"}
	for _, bad := range []func(c *Config){
		func(c *Config) { c.SMTP = "mail.example.org:587" },
		func(c *Config) { c.IMAP = "192.0.2.1:143" },
		func(c *Config) { c.URL = "https://mail.example.org" },
		func(c *Config) { c.IMAP = "" }, // would be the server's :993
	} {
		c := base
		bad(&c)
		c.Server = "mail.example.org"
		if err := c.Defaults(); err == nil {
			t.Errorf("stand_in accepted %s %s %s", c.SMTP, c.IMAP, c.URL)
		}
	}
	c := base
	if err := c.Defaults(); err != nil || !c.PlainIMAP || !c.PlainSMTP {
		t.Fatalf("loopback stand-in: %v %+v", err, c)
	}
}

// A server throttle that drops one connection is tried again, not a miss:
// the SMTP sign-in (connection 1) and the send (connection 3, after the
// retried sign-in's 2) each lose one connection here.
func TestDroppedConnectionIsRetried(t *testing.T) {
	f := newFakeMail(t)
	w := watcher(t, f, f.Password)
	f.RefuseSMTP(1, 3)
	w.Round(context.Background())
	s := w.Snapshot()
	for _, si := range s.SignIns {
		if !si.OK {
			t.Errorf("%s sign-in failed: %s", si.Name, si.Error)
		}
	}
	if !s.Trip.OK {
		t.Errorf("trip = %+v, want it to arrive after one retried send", s.Trip)
	}
}

// Dropped twice in a row is a real failure, reported as before.
func TestSendDroppedTwiceFails(t *testing.T) {
	f := newFakeMail(t)
	w := watcher(t, f, f.Password)
	f.RefuseSMTP(2, 3) // connection 1 is the sign-in; both tries at sending drop
	w.Round(context.Background())
	s := w.Snapshot()
	if s.Trip.OK || !strings.HasPrefix(s.Trip.Error, "sending: ") || !strings.Contains(s.Trip.Error, "EOF") {
		t.Errorf("trip = %+v, want a sending failure mentioning EOF", s.Trip)
	}
	if s.Trip.Fails != 1 {
		t.Errorf("fails = %d, want 1", s.Trip.Fails)
	}
}

func TestTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, true},
		{fmt.Errorf("STARTTLS: %w", io.ErrUnexpectedEOF), true},
		{&textproto.Error{Code: 421, Msg: "too many connections"}, true},
		{fmt.Errorf("sign-in refused: %w", &textproto.Error{Code: 535, Msg: "bad credentials"}), false},
		{errors.New("connection refused"), false},
	}
	for _, c := range cases {
		if got := transient(c.err); got != c.want {
			t.Errorf("transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
