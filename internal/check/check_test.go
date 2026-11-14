package check

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func spec(t *testing.T, s Spec) Spec {
	t.Helper()
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecValidation(t *testing.T) {
	for _, bad := range []Spec{
		{},
		{ID: "two", HTTP: "https://a.test/", TCP: "a.test:25"},
		{ID: "none"},
		{ID: "fast", HTTP: "https://a.test/", Every: time.Second},
		{ID: "ftp", HTTP: "ftp://a.test/"},
		{ID: "port", TCP: "a.test"},
		{ID: "type", DNS: "a.test", Type: "SRV"},
		{ID: "sev", Ping: "a.test", Severity: "panic"},
		{ID: "slow", Ping: "a.test", Every: 30 * time.Second, Timeout: time.Minute},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	s := spec(t, Spec{ID: "x", DNS: "a.test", Type: "mx"})
	if s.Type != "MX" || s.Every != time.Minute || s.Severity != "critical" || s.TLSWarnDays != 21 || s.Name != "x" {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if !strings.HasPrefix(r.UserAgent(), "obs-beacon-check/") {
				t.Errorf("user agent %q", r.UserAgent())
			}
			w.Write([]byte("<title>Coffey Labs</title>"))
		case "/moved":
			http.Redirect(w, r, "/", http.StatusMovedPermanently)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if r := Run(ctx, spec(t, Spec{ID: "ok", HTTP: srv.URL + "/", Contains: "Coffey Labs"})); !r.OK || r.Status != 200 || r.Ms <= 0 {
		t.Fatalf("ok: %+v", r)
	}
	if r := Run(ctx, spec(t, Spec{ID: "text", HTTP: srv.URL + "/", Contains: "inbuxa"})); r.OK || !strings.Contains(r.Error, `doesn't contain "inbuxa"`) {
		t.Fatalf("missing text: %+v", r)
	}
	// Redirects are answers, not followed.
	if r := Run(ctx, spec(t, Spec{ID: "moved", HTTP: srv.URL + "/moved", Status: 301})); !r.OK || r.Status != 301 {
		t.Fatalf("redirect: %+v", r)
	}
	if r := Run(ctx, spec(t, Spec{ID: "moved200", HTTP: srv.URL + "/moved"})); r.OK || r.Error != "HTTP 301, expected 200" {
		t.Fatalf("redirect as failure: %+v", r)
	}
	if r := Run(ctx, spec(t, Spec{ID: "slow", HTTP: srv.URL + "/slow", Timeout: 100 * time.Millisecond, Every: time.Minute})); r.OK || r.Error != "timed out" {
		t.Fatalf("timeout: %+v", r)
	}
	// A certificate nobody vouches for fails, and the error says why.
	tsrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tsrv.Close()
	if r := Run(ctx, spec(t, Spec{ID: "tls", HTTP: tsrv.URL + "/"})); r.OK || !strings.Contains(r.Error, "certificate") {
		t.Fatalf("untrusted cert: %+v", r)
	}
}

func TestTCPBanner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	quit := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("220 mail.example ESMTP ready\r\n"))
			buf := make([]byte, 16)
			c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf)
			quit <- string(buf[:n])
			c.Close()
		}
	}()
	ctx := context.Background()
	r := Run(ctx, spec(t, Spec{ID: "smtp", TCP: ln.Addr().String(), Contains: "ESMTP"}))
	if !r.OK || r.Answer != "220 mail.example ESMTP ready" {
		t.Fatalf("banner: %+v", r)
	}
	if got := <-quit; got != "QUIT\r\n" {
		t.Fatalf("didn't say goodbye: %q", got)
	}
	if r := Run(ctx, spec(t, Spec{ID: "imap", TCP: ln.Addr().String(), Contains: "* OK"})); r.OK {
		t.Fatalf("wrong banner passed: %+v", r)
	}
	<-quit
	if r := Run(ctx, spec(t, Spec{ID: "plain", TCP: ln.Addr().String()})); !r.OK {
		t.Fatalf("connect only: %+v", r)
	}
	ln.Close()
	if r := Run(ctx, spec(t, Spec{ID: "closed", TCP: ln.Addr().String()})); r.OK || r.Error != "connection refused" {
		t.Fatalf("closed port: %+v", r)
	}
}

func TestDNSAndPingLocal(t *testing.T) {
	ctx := context.Background()
	if r := Run(ctx, spec(t, Spec{ID: "dns", DNS: "localhost", Contains: "127.0.0.1"})); !r.OK {
		t.Fatalf("dns: %+v", r)
	}
	if r := Run(ctx, spec(t, Spec{ID: "nx", DNS: "nothing.invalid"})); r.OK {
		t.Fatalf("nx: %+v", r)
	}
	r := Run(ctx, spec(t, Spec{ID: "ping", Ping: "127.0.0.1", Timeout: 3 * time.Second}))
	if !r.OK && strings.Contains(r.Error, "can't open a ping socket") {
		t.Skip("this machine doesn't allow unprivileged ping:", r.Error)
	}
	if !r.OK || r.Loss != 0 || r.Ms <= 0 {
		t.Fatalf("ping: %+v", r)
	}
}

func TestRunnerSetReplaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRunner(ctx)
	a := spec(t, Spec{ID: "a", HTTP: srv.URL + "/", Every: 30 * time.Second, Timeout: time.Second})
	r.Set([]Spec{a})
	// The first run is spread over the interval, so run it directly here.
	r.mu.Lock()
	r.results["a"] = Run(ctx, a)
	r.mu.Unlock()
	if res := r.Results(); len(res) != 1 || !res[0].OK {
		t.Fatalf("results: %+v", res)
	}
	r.Set(nil)
	if res := r.Results(); len(res) != 0 {
		t.Fatalf("removed check kept results: %+v", res)
	}
}
