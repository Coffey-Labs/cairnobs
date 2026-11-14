package state

import (
	"strings"
	"testing"
	"time"
)

func TestCeremoniesAreSealedAndUsedOnce(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	if err := db.PutCeremony("oauth", "state-abc", []byte(`{"verifier":"very-secret"}`), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var k, v string
	_ = db.queryRow(`SELECT k, v FROM ceremonies`).Scan(&k, &v)
	if strings.Contains(k+v, "state-abc") || strings.Contains(v, "very-secret") {
		t.Fatalf("stored in the clear: %s %s", k, v)
	}
	if b, err := db.Ceremony("oauth", "state-abc", now); err != nil || !strings.Contains(string(b), "very-secret") {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := db.Ceremony("passkey", "state-abc", now); err != ErrNotFound {
		t.Fatalf("another kind: %v", err)
	}
	if b, err := db.TakeCeremony("oauth", "state-abc", now); err != nil || len(b) == 0 {
		t.Fatalf("take %q %v", b, err)
	}
	if _, err := db.TakeCeremony("oauth", "state-abc", now); err != ErrNotFound {
		t.Fatalf("taken twice: %v", err)
	}
	// Expired ones read as gone, and go when the next is stored.
	_ = db.PutCeremony("oauth", "old", []byte("x"), now.Add(-time.Second))
	if _, err := db.Ceremony("oauth", "old", now); err != ErrNotFound {
		t.Fatalf("expired: %v", err)
	}
	if _, err := db.TakeCeremony("oauth", "old", now); err != ErrNotFound {
		t.Fatalf("expired take: %v", err)
	}
	_ = db.PutCeremony("oauth", "old2", []byte("x"), now.Add(-time.Second))
	_ = db.PutCeremony("totp", "new", []byte("x"), now.Add(time.Minute))
	if n := db.count(t, "ceremonies"); n != 1 {
		t.Fatalf("%d ceremonies left, want 1", n)
	}
	_ = db.DropCeremony("totp", "new")
	if n := db.count(t, "ceremonies"); n != 0 {
		t.Fatal("not dropped")
	}
}

func TestSigninFailures(t *testing.T) {
	db, _ := open(t)
	now := time.Now()
	for i := range 3 {
		at := now.Add(time.Duration(i-10) * time.Minute)
		if err := db.AddSigninFailure(at, now.Add(-time.Hour), "acct:a@example.test", "ip:192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.SigninFailures(now.Add(-9*time.Minute-30*time.Second), "acct:a@example.test", "ip:192.0.2.1", "acct:b@example.test")
	if err != nil || got["acct:a@example.test"] != 2 || got["ip:192.0.2.1"] != 2 || got["acct:b@example.test"] != 0 {
		t.Fatalf("%v %v", got, err)
	}
	var k string
	_ = db.queryRow(`SELECT k FROM signin_failures`).Scan(&k)
	if strings.Contains(k, "example") || strings.Contains(k, "192.0.2") {
		t.Fatalf("stored in the clear: %s", k)
	}
	_ = db.ClearSigninFailures("acct:a@example.test")
	if got, _ := db.SigninFailures(now.Add(-time.Hour), "acct:a@example.test", "ip:192.0.2.1"); got["acct:a@example.test"] != 0 || got["ip:192.0.2.1"] != 3 {
		t.Fatalf("after clear: %v", got)
	}
	// Old failures are forgotten as new ones come in.
	_ = db.AddSigninFailure(now, now.Add(-time.Minute), "ip:192.0.2.1")
	if n := db.count(t, "signin_failures"); n != 1 {
		t.Fatalf("%d failures kept, want 1", n)
	}
}
