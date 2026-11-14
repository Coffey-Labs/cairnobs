package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// An install moves from SQLite to PostgreSQL with everything it had, and
// reads back the same through the same key.
func TestCopySQLiteToPostgres(t *testing.T) {
	postgresOnly(t)
	dir, kf := t.TempDir(), testKey(t)
	path := filepath.Join(dir, "beacon.db")
	src, err := Open(path, kf)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	old := now.AddDate(0, -5, 0) // history months back, so it spans partitions
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(src.PutSession(Session{TokenSHA256: "tok", User: "Pat", Email: "pat@example.test", Role: "admin", Provider: "oidc", Dev: true, Expires: now.Add(time.Hour), Checked: now, Sealed: src.Seal([]byte("refresh"))}))
	must(src.PutIncident(Incident{ID: "B-0001", Key: "k", Severity: "critical", Title: "disk", Opened: old, Fired: old.Add(time.Minute), Resolved: old.Add(time.Hour), AckBy: "Pat", AckAt: old.Add(2 * time.Minute)}))
	must(src.PutIncident(Incident{ID: "B-0002", Key: "k2", Severity: "warning", Title: "load", Opened: now}))
	must(src.SetNote("B-0001", Note{Text: "replaced it", By: "Pat", At: old}))
	must(src.SetMeta("incident_seq", "2"))
	must(src.SetAgentCA([]byte("-----BEGIN CERTIFICATE-----")))
	for i, a := range []string{"sign-in", "ack", "note"} {
		must(src.Audit(Entry{At: old.Add(time.Duration(i) * 24 * time.Hour), Who: "Pat", Action: a}))
	}
	must(src.PutUser(User{ID: "u1", Name: "Robin", Email: "robin@example.test", Role: "viewer", Created: now, Emergency: true}))
	must(src.PutPasskey(Passkey{ID: "c1", UserID: "u1", Name: "Laptop", Credential: "{}", Created: now}))
	must(src.PutSetupLink("link", "u1", now.Add(time.Hour)))
	must(src.LogStatus("svc/mail", "good", old))
	must(src.LogStatus("svc/mail", "critical", now.Add(-time.Hour)))
	must(src.LogStatus("svc/git", "good", old.AddDate(0, 1, 0)))
	must(src.PutHostHour(HostHour{Host: "h1", Hour: old, CPU: 2.5, CPUMax: 9, Mem: 40, MemMax: 41, Disks: []DiskSample{{Mount: "/", Used: 1 << 40, Total: 2 << 40}}}))
	must(src.PutMailRound(MailRound{At: now.Add(-time.Hour), Seconds: 0.75, Queue: 3}))
	must(src.PutSetting("checks", Setting{YAML: "checks: []\n", Updated: now, UpdatedBy: "Pat"}))
	must(src.AddJoinToken(JoinToken{TokenSHA256: "jt", Agent: "web1", Workstation: true, Created: now, CreatedBy: "Pat", Expires: now.Add(time.Hour)}))
	if _, err := src.UseJoinToken("jt", now, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	must(src.IssueAgentCert(AgentCert{Serial: "01", Agent: "web1", Issued: now, NotAfter: now.AddDate(0, 3, 0)}))
	src.Close()

	to := statetest.Database(t, dir)
	copied, err := Copy(path, to, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int64{}
	for _, c := range copied {
		rows[c.Table] = c.Rows
	}
	if rows["audit"] != 3 || rows["status_log"] != 3 || rows["users"] != 1 || rows["agent_certs"] != 1 || len(copied) != len(copyTables) {
		t.Fatalf("copied %+v", copied)
	}

	db, err := Open(to, kf)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := db.count(t, "status_log_default") + db.count(t, "host_hours_default") + db.count(t, "audit_default"); n != 0 {
		t.Errorf("%d copied rows in default partitions", n)
	}
	ss, _ := db.Sessions(now)
	if len(ss) != 1 || !ss[0].Dev || ss[0].User != "Pat" {
		t.Fatalf("sessions %+v", ss)
	}
	if b, err := db.Unseal(ss[0].Sealed); err != nil || string(b) != "refresh" {
		t.Fatalf("unseal %q %v", b, err)
	}
	if ca, err := db.AgentCA(); err != nil || string(ca) != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("agent CA %q %v", ca, err)
	}
	open, resolved, _ := db.Incidents(10)
	if len(open) != 1 || len(resolved) != 1 || resolved[0].AckBy != "Pat" || !resolved[0].Fired.Equal(old.Add(time.Minute)) {
		t.Fatalf("incidents %+v %+v", open, resolved)
	}
	if log, _ := db.AuditLog(10); len(log) != 3 || log[0].Action != "note" || log[2].Action != "sign-in" {
		t.Fatalf("audit %+v", log)
	}
	if u, err := db.GetUser("u1"); err != nil || !u.Emergency || u.Name != "Robin" {
		t.Fatalf("user %+v %v", u, err)
	}
	if ps, _ := db.Passkeys("u1"); len(ps) != 1 {
		t.Fatalf("passkeys %+v", ps)
	}
	if latest, _ := db.LatestStatuses(); latest["svc/mail"] != "critical" || latest["svc/git"] != "good" {
		t.Fatalf("latest %v", latest)
	}
	if hh, _ := db.HostHours(old, old.Add(time.Hour)); len(hh) != 1 || hh[0].CPU != 2.5 || hh[0].Disks[0].Used != 1<<40 {
		t.Fatalf("host hours %+v", hh)
	}
	if n, _ := db.Notes([]string{"B-0001"}); n["B-0001"].By != "Pat" {
		t.Fatalf("notes %+v", n)
	}
	if ja, _ := db.JoinedAgents(); len(ja) != 1 || !ja[0].Workstation {
		t.Fatalf("agents %+v", ja)
	}
	if s, ok, _ := db.Setting("checks"); !ok || s.UpdatedBy != "Pat" {
		t.Fatalf("setting %+v", s)
	}
	// New audit entries number on after the copied ones.
	_ = db.Audit(Entry{At: now, Who: "Pat", Action: "after"})
	if log, _ := db.AuditLog(1); log[0].Action != "after" {
		t.Fatalf("newest audit entry %+v", log)
	}

	// Only ever into an empty database.
	if _, err := Copy(path, to, Options{}); err == nil {
		t.Fatal("copied into a database that has data")
	}
}

func TestCopyRefusesTheWrongWayRound(t *testing.T) {
	if _, err := Copy("postgres://x/y", "postgres://x/z", Options{}); err == nil {
		t.Fatal("copied from PostgreSQL")
	}
	if _, err := Copy(filepath.Join(t.TempDir(), "a.db"), filepath.Join(t.TempDir(), "b.db"), Options{}); err == nil {
		t.Fatal("copied into SQLite")
	}
	if _, err := Copy(filepath.Join(t.TempDir(), "missing.db"), "postgres://x/z", Options{}); err == nil {
		t.Fatal("copied from a file that isn't there")
	}
}
