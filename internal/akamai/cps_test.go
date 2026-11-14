package akamai

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func pemCert(t *testing.T, cn string, rsaKey bool, left time.Duration) string {
	t.Helper()
	p := FakePEM("Test CA", []string{cn}, !rsaKey, time.Now().Add(left))
	if p == "" {
		t.Fatal("no certificate")
	}
	return p
}

// Step 2: CPS enrollments through the contracts Property Manager lists,
// with the older media types the fake speaks; the deployed certificates
// (RSA and ECDSA), and what a pending change waits for.
func TestCPS(t *testing.T) {
	c := testCreds()
	f := &Fake{Creds: c}
	f.SetEnrollment(1001, FakeEnrollment{CN: "www.example.com", SANs: []string{"www.example.com", "example.com"}, Validation: "dv", Contract: "1-ABC",
		Deployed: []string{pemCert(t, "www.example.com", true, 60*24*time.Hour), pemCert(t, "www.example.com", false, 60*24*time.Hour)}, Waiting: "-"})
	f.SetEnrollment(1002, FakeEnrollment{CN: "shop.example.com", Validation: "third-party", Contract: "1-ABC",
		Deployed: []string{pemCert(t, "shop.example.com", true, 10*24*time.Hour)}, Waiting: "third-party-csr"})
	f.SetEnrollment(1003, FakeEnrollment{CN: "new.example.com", Validation: "dv", Contract: "2-DEF"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, _ := New(Config{Edgerc: writeEdgerc(t, c), API: srv.URL})
	w.Poll(context.Background(), time.Now())
	s := w.Snapshot()
	if s.Error != "" || len(s.Enrollments) != 3 {
		t.Fatalf("%+v", s)
	}
	by := map[string]Enrollment{}
	for _, e := range s.Enrollments {
		by[e.CN] = e
	}
	if e := by["www.example.com"]; len(e.Deployed) != 2 || e.Deployed[0].Key != "RSA 2048" || e.Deployed[1].Key != "ECDSA P-256" || e.Pending != 0 || len(e.SANs) != 2 {
		t.Errorf("www: %+v", e)
	}
	if e := by["shop.example.com"]; len(e.Deployed) != 1 || e.Pending != 1 || e.Waiting != "third-party-csr" || e.Validation != "third-party" {
		t.Errorf("shop: %+v", e)
	}
	if e := by["new.example.com"]; len(e.Deployed) != 0 || e.Pending != 1 || e.Waiting != "" || e.Error != "" {
		t.Errorf("new: %+v", e)
	}

	f.SetEnrollment(1004, FakeEnrollment{CN: "later.example.com", Validation: "dv", Contract: "2-DEF"})
	w.Poll(context.Background(), time.Now().Add(5*time.Minute))
	if s := w.Snapshot(); len(s.Enrollments) != 3 {
		t.Errorf("CPS read again within 15 minutes: %d", len(s.Enrollments))
	}
	w.Poll(context.Background(), time.Now().Add(16*time.Minute))
	if s := w.Snapshot(); len(s.Enrollments) != 4 {
		t.Errorf("CPS not read again after 15 minutes: %d", len(s.Enrollments))
	}

	f.NoCPS = true
	w.Poll(context.Background(), time.Now().Add(32*time.Minute))
	if s := w.Snapshot(); s.Enrollments != nil || len(s.Absent) != 1 || s.Absent[0] != "cps" {
		t.Errorf("without CPS: %+v", s)
	}
}
