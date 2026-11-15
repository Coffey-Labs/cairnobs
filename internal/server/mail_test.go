package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/mailmon"
)

func mailServer(t *testing.T, s mailmon.Snapshot) *Server {
	t.Helper()
	srv, _ := testServer(t, true)
	pf := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pf, []byte("x"), 0o600)
	w, err := mailmon.New(mailmon.Config{Account: "monitor@example.test", PasswordFile: pf, Server: "mail.example.test",
		ClientID: "cairn-obs", RedirectURL: "https://obs.example.test/cb", Host: "node1"})
	if err != nil {
		t.Fatal(err)
	}
	w.Set(s)
	srv.mail = w
	return srv
}

func TestMailConditions(t *testing.T) {
	now := time.Now()
	ok := []mailmon.SignIn{{Name: "SMTP", OK: true}, {Name: "IMAP", OK: true}, {Name: "JMAP", OK: true}}
	old := now.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name string
		s    mailmon.Snapshot
		want string
	}{
		{"before the first round", mailmon.Snapshot{}, ""},
		{"all well", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{OK: true, Seconds: 1.2}, Queue: mailmon.Queue{Read: true, Total: 3}}, ""},
		{"one missed message warns", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{Error: "sent, but it hadn't arrived after 4m0s", Fails: 1}, Queue: mailmon.Queue{Read: true}},
			"warning:mail/roundtrip"},
		{"two in a row is critical", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{Error: "x", Fails: 2}, Queue: mailmon.Queue{Read: true}},
			"critical:mail/roundtrip"},
		{"slow", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{OK: true, Seconds: 75}, Queue: mailmon.Queue{Read: true}},
			"warning:mail/roundtrip"},
		{"queue", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{OK: true, Seconds: 1}, Queue: mailmon.Queue{Read: true, Total: 60, Oldest: &old}},
			"warning:mail/queue"},
		{"queue critical", mailmon.Snapshot{Checked: now, SignIns: ok, Trip: mailmon.Trip{OK: true, Seconds: 1}, Queue: mailmon.Queue{Read: true, Total: 500}},
			"critical:mail/queue"},
		{"IMAP refused once is quiet", mailmon.Snapshot{Checked: now, SignIns: []mailmon.SignIn{{Name: "SMTP", OK: true}, {Name: "IMAP", Error: "sign-in refused", Fails: 1}, {Name: "JMAP", OK: true}},
			Trip: mailmon.Trip{OK: true, Seconds: 1}, Queue: mailmon.Queue{Read: true}}, ""},
		{"JMAP down speaks for the trip and queue", mailmon.Snapshot{Checked: now,
			SignIns: []mailmon.SignIn{{Name: "SMTP", Error: "refused", Fails: 2}, {Name: "IMAP", Error: "refused", Fails: 2}, {Name: "JMAP", Error: "refused", Fails: 2}},
			Trip:    mailmon.Trip{Error: "JMAP sign-in failed", Fails: 2}, Queue: mailmon.Queue{Error: "JMAP sign-in failed", Fails: 2}},
			"critical:mail/signin/smtp critical:mail/signin/imap critical:mail/signin/jmap"},
		{"stopped", mailmon.Snapshot{Checked: now.Add(-time.Hour), SignIns: ok, Trip: mailmon.Trip{OK: true, Seconds: 1}, Queue: mailmon.Queue{Read: true}},
			"warning:mail/stale"},
	} {
		srv := mailServer(t, tc.s)
		conds := srv.mailConditions(now)
		if got := keys(conds); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		for _, c := range conds {
			if c.Host != "node1" || c.Href != "/mail" {
				t.Errorf("%s: %s has host %q href %q; the maintenance window must hold it", tc.name, c.Key, c.Host, c.Href)
			}
		}
	}
}

func TestMailGroupsUnderMailNode(t *testing.T) {
	top := &Topology{Services: []ServiceSpec{{ID: "mail", Name: "Mail"}}}
	if n := nodeOf(top, Condition{Key: "mail/roundtrip"}); n != "mail" {
		t.Errorf("nodeOf(mail/roundtrip) = %q, want mail", n)
	}
}
