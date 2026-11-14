package state

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kf := filepath.Join(dir, "key")
	_ = os.WriteFile(kf, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600)
	db, err := Open(statetest.Database(t, dir), kf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func TestSealHidesAndRoundTrips(t *testing.T) {
	db, dir := open(t)
	sealed := db.Seal([]byte(`{"refresh":"very-secret-refresh-token"}`))
	if strings.Contains(sealed, "very-secret") {
		t.Fatal("sealed value shows the secret")
	}
	if err := db.PutSession(Session{TokenSHA256: "abc", User: "Pat Admin", Role: "admin", Provider: "inbuxa", Expires: time.Now().Add(time.Hour), Checked: time.Now(), Sealed: sealed}); err != nil {
		t.Fatal(err)
	}
	var stored string
	_ = db.queryRow(`SELECT sealed FROM sessions WHERE token_sha256 = 'abc'`).Scan(&stored)
	if stored != sealed {
		t.Fatalf("stored %q", stored)
	}
	if statetest.Postgres() {
		return
	}
	// Nothing secret in the file on disk either.
	_ = db.db.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "beacon.db"))
	wal, _ := os.ReadFile(filepath.Join(dir, "beacon.db-wal"))
	if strings.Contains(string(raw)+string(wal), "very-secret") {
		t.Fatal("the database file holds the secret in the clear")
	}
}

func TestSessionsSurviveAndExpire(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	_ = db.PutSession(Session{TokenSHA256: "live", User: "a", Expires: now.Add(time.Hour), Checked: now, Sealed: db.Seal([]byte("x"))})
	_ = db.PutSession(Session{TokenSHA256: "old", User: "b", Expires: now.Add(-time.Minute), Checked: now, Sealed: db.Seal([]byte("x"))})
	got, err := db.Sessions(now)
	if err != nil || len(got) != 1 || got[0].TokenSHA256 != "live" {
		t.Fatalf("sessions %+v %v", got, err)
	}
	b, err := db.Unseal(got[0].Sealed)
	if err != nil || string(b) != "x" {
		t.Fatalf("unseal %q %v", b, err)
	}
	_ = db.DeleteSession("live")
	if got, _ := db.Sessions(now); len(got) != 0 {
		t.Fatal("session not deleted")
	}
}

func TestIncidentsOpenAndResolved(t *testing.T) {
	db, _ := open(t)
	t0 := time.Unix(1792000000, 0)
	_ = db.PutIncident(Incident{ID: "B-0001", Key: "k1", Severity: "critical", Title: "a", Opened: t0, Fired: t0.Add(2 * time.Minute)})
	_ = db.PutIncident(Incident{ID: "B-0002", Key: "k2", Severity: "warning", Title: "b", Opened: t0, Grouping: `{"affected":"webmail"}`})
	if ok, err := db.AckIncident("B-0001", "Pat Admin", t0.Add(5*time.Minute)); !ok || err != nil {
		t.Fatalf("ack: %v %v", ok, err)
	}
	// Saving it again (resolved) keeps the acknowledgement.
	_ = db.PutIncident(Incident{ID: "B-0001", Key: "k1", Severity: "critical", Title: "a", Opened: t0, Fired: t0.Add(2 * time.Minute), Resolved: t0.Add(time.Hour)})
	if ok, _ := db.AckIncident("B-0001", "late", t0); ok {
		t.Fatal("acknowledged a resolved incident")
	}
	open, resolved, err := db.Incidents(10)
	if err != nil || len(open) != 1 || open[0].ID != "B-0002" || !open[0].Fired.IsZero() || open[0].Grouping != `{"affected":"webmail"}` {
		t.Fatalf("open %+v %v", open, err)
	}
	if len(resolved) != 1 || resolved[0].AckBy != "Pat Admin" || !resolved[0].Resolved.Equal(t0.Add(time.Hour)) {
		t.Fatalf("resolved %+v", resolved)
	}
	_ = db.SetMeta("incident_seq", "2")
	if db.Meta("incident_seq") != "2" || db.Meta("nope") != "" {
		t.Fatal("meta")
	}
}

func TestAuditAndBackup(t *testing.T) {
	db, dir := open(t)
	_ = db.Audit(Entry{At: time.Now(), Who: "Pat Admin", Action: "sign-in", Detail: "inbuxa"})
	_ = db.Audit(Entry{At: time.Now(), Who: "Pat Admin", Action: "ack", Detail: "B-0001"})
	log, err := db.AuditLog(10)
	if err != nil || len(log) != 2 || log[0].Action != "ack" {
		t.Fatalf("audit %+v %v", log, err)
	}
	bak := filepath.Join(dir, "backup.db")
	if statetest.Postgres() {
		if err := db.Backup(bak); err == nil {
			t.Fatal("a PostgreSQL database wrote a SQLite snapshot")
		}
		return
	}
	if err := db.Backup(bak); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(bak); err != nil || st.Size() == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("backup %v %v", st.Mode(), err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "beacon.db")); st.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v", st.Mode())
	}
}

func TestBadKeyRefused(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "key")
	_ = os.WriteFile(kf, []byte("too-short"), 0o600)
	if _, err := Open(filepath.Join(dir, "x.db"), kf); err == nil {
		t.Fatal("accepted a bad key")
	}
}

func TestUsersPasskeysAndSetupLinks(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	u := User{ID: "u1", Name: "Robin", Email: "robin@example.test", Role: "viewer", Created: now}
	if err := db.PutUser(u); err != nil {
		t.Fatal(err)
	}
	if err := db.PutUser(User{ID: "u2", Name: "Other", Email: "robin@example.test", Role: "viewer", Created: now}); err == nil {
		t.Fatal("two accounts with one email")
	}
	if err := db.PutSetupLink("tokhash", "u1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if uid, err := db.SetupLink("tokhash", now); err != nil || uid != "u1" || !db.PendingSetup("u1", now) {
		t.Fatalf("setup link: %q %v", uid, err)
	}
	if _, err := db.SetupLink("tokhash", now.Add(2*time.Hour)); err != ErrNotFound {
		t.Fatalf("expired link: %v", err)
	}
	_ = db.PutPasskey(Passkey{ID: "cred1", UserID: "u1", Name: "Laptop", Credential: `{"id":"x"}`, Created: now})
	if ps, _ := db.Passkeys("u1"); len(ps) != 1 || ps[0].Name != "Laptop" {
		t.Fatalf("passkeys %+v", ps)
	}
	if err := db.DeletePasskey("someone-else", "cred1"); err != ErrNotFound {
		t.Fatalf("deleted another user's passkey: %v", err)
	}
	// Deleting the account takes its passkeys and links with it.
	_ = db.DeleteUser("u1")
	if ps, _ := db.Passkeys("u1"); len(ps) != 0 || db.PendingSetup("u1", now) {
		t.Fatal("leftovers after deleting the user")
	}
	if _, err := db.GetUser("u1"); err != ErrNotFound {
		t.Fatalf("deleted user: %v", err)
	}
}

func TestUsersTableMigrates(t *testing.T) {
	statetest.SQLiteOnly(t)
	dir := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kf := filepath.Join(dir, "key")
	_ = os.WriteFile(kf, []byte(base64.StdEncoding.EncodeToString(key)), 0o600)
	// A database from before the emergency column, and from before
	// migrations were numbered.
	db, err := Open(filepath.Join(dir, "beacon.db"), kf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.db.Exec(`DROP TABLE schema_migrations`)
	_, _ = db.db.Exec(`DROP TABLE users`)
	_, _ = db.db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL, created INTEGER NOT NULL, disabled INTEGER NOT NULL DEFAULT 0, last_sign_in INTEGER)`)
	_, _ = db.db.Exec(`INSERT INTO users VALUES ('u1', 'Old', 'old@example.test', 'admin', 1, 0, NULL)`)
	db.Close()
	db, err = Open(filepath.Join(dir, "beacon.db"), kf)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := db.GetUser("u1")
	if err != nil || u.Emergency || u.Name != "Old" {
		t.Fatalf("%+v %v", u, err)
	}
	u.Emergency = true
	_ = db.PutUser(u)
	if u, _ := db.GetUser("u1"); !u.Emergency {
		t.Fatal("emergency not stored")
	}
}
