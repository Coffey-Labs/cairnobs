package localauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/api/authz"
)

func TestLoginLimiterAllowsUpToMax(t *testing.T) {
	l := newLoginLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d: want allowed", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("4th attempt within the window: want denied")
	}
}

func TestLoginLimiterIsPerKey(t *testing.T) {
	l := newLoginLimiter(1, time.Minute)
	if !l.allow("1.2.3.4") {
		t.Fatal("first attempt from 1.2.3.4: want allowed")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("a different IP must have its own budget")
	}
	if l.allow("1.2.3.4") {
		t.Fatal("second attempt from 1.2.3.4: want denied")
	}
}

func TestLoginLimiterResetsAfterWindow(t *testing.T) {
	l := newLoginLimiter(1, 10*time.Millisecond)
	if !l.allow("1.2.3.4") {
		t.Fatal("first attempt: want allowed")
	}
	if l.allow("1.2.3.4") {
		t.Fatal("second attempt within the window: want denied")
	}
	time.Sleep(20 * time.Millisecond)
	if !l.allow("1.2.3.4") {
		t.Fatal("attempt after the window elapsed: want allowed")
	}
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP() = %q, want %q", got, "203.0.113.9")
	}
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = "198.51.100.7:5555"
	if got := clientIP(r); got != "198.51.100.7" {
		t.Errorf("clientIP() = %q, want %q", got, "198.51.100.7")
	}
}

// TestHandleLoginRateLimited is the regression test for the
// security-audit finding that POST /auth/login had no rate limiting at
// all -- repeated attempts from the same client must eventually get a
// 429, not another 401.
func TestHandleLoginRateLimited(t *testing.T) {
	fs := newFakeStore()
	mustCreateUser(t, fs, "alice", "hunter22", authz.RoleEditor)
	_, mux := newTestHandler(t, fs)

	var last *httptest.ResponseRecorder
	for i := 0; i < loginRateLimitMax+1; i++ {
		last = doRequest(t, mux, http.MethodPost, "/auth/login", `{"username":"alice","password":"wrong-password"}`, nil)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after exceeding the limit = %d, want 429", last.Code)
	}
}
