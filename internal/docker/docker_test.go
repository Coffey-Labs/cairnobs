package docker

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestAllowed(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/containers/json", true},
		{"GET", "/v1.47/containers/json", true},
		{"GET", "/containers/3f2a9c/json", true},
		{"GET", "/v1.47/containers/gitea-db/json", true},
		{"GET", "/info", true},
		{"GET", "/system/df", true},
		{"HEAD", "/_ping", true},

		{"POST", "/containers/json", false},
		{"POST", "/containers/abc/start", false},
		{"POST", "/containers/abc/exec", false},
		{"DELETE", "/containers/abc", false},
		{"GET", "/containers/abc/logs", true}, // path only; the gate also checks the query
		{"POST", "/containers/abc/logs", false},
		{"GET", "/containers/abc/archive", false},
		{"GET", "/containers/abc/export", false},
		{"GET", "/containers/../exec/x/json", false},
		{"GET", "/containers//json", false},
		{"GET", "/containers/json/", false},
		{"GET", "/v1.47/exec/abc/json", false},
		{"GET", "/events", false},
		{"GET", "/secrets", false},
		{"GET", "", false},
	}
	for _, c := range cases {
		if got := Allowed(c.method, c.path); got != c.want {
			t.Errorf("Allowed(%s %q) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestGateRefusesBeforeDialing(t *testing.T) {
	// The socket doesn't exist: a refused request must never get as far as dialing.
	g := Gate("/nonexistent/docker.sock")
	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/containers/abc/start", nil),
		httptest.NewRequest("GET", "/containers/abc/logs", nil), // no tail
		httptest.NewRequest("GET", "/containers/abc/logs?tail=20&follow=1", nil),
		httptest.NewRequest("GET", "/containers/abc/logs?tail=500", nil),
		httptest.NewRequest("GET", "/containers/abc/logs?tail=all", nil),
		httptest.NewRequest("GET", "/v1.47/containers/abc/logs?tail=10&since=0", nil),
		httptest.NewRequest("GET", "/containers/a%2Fb/json", nil),
	} {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: got %d, want 403", req.Method, req.URL, rec.Code)
		}
	}
}

func TestGateWithoutDocker(t *testing.T) {
	// An allowed read on a host with no Docker: "no Docker here", which the
	// client turns into ErrNoDocker (no containers field at all).
	sock := filepath.Join(t.TempDir(), "helper.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Gate(filepath.Join(t.TempDir(), "docker.sock"))}
	go srv.Serve(ln)
	defer srv.Close()
	if _, err := NewClient(sock).Containers(t.Context()); !errors.Is(err, ErrNoDocker) {
		t.Errorf("Containers: %v, want ErrNoDocker", err)
	}
}

func TestHealthFromStatus(t *testing.T) {
	for in, want := range map[string]string{
		"Up 3 hours (healthy)":            "healthy",
		"Up 2 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 4 days":                       "",
		"Exited (137) 2 minutes ago":      "",
	} {
		if got := healthFromStatus(in); got != want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogsQuery(t *testing.T) {
	q, ok := logsQuery(map[string][]string{"tail": {"20"}, "stdout": {"1"}, "stderr": {"0"}})
	if !ok || q != "stdout=1&stderr=1&timestamps=1&tail=20" {
		t.Fatalf("got %q %v", q, ok)
	}
}

func TestDemuxAndSplit(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		b := []byte{stream, 0, 0, 0, 0, 0, 0, byte(len(s))}
		return append(b, s...)
	}
	raw := append(frame(1, "2026-10-15T21:00:00Z starting\n"), frame(2, "2026-10-15T21:00:01Z panic: db down\n")...)
	got := splitLog(demux(raw), 20)
	if len(got) != 2 || got[1] != "2026-10-15T21:00:01Z panic: db down" {
		t.Fatalf("multiplexed: %q", got)
	}
	// A TTY container's log has no frames.
	if got := splitLog(demux([]byte("a\nb\nc\n")), 2); len(got) != 2 || got[0] != "b" {
		t.Fatalf("plain: %q", got)
	}
}

func TestTidy(t *testing.T) {
	got := tidy([]string{"2026-10-15T21:50:04.540791178Z error", "2026-10-15T21:50:04.54078Z starting", "no timestamp"})
	if got[0] != "2026-10-15 21:50:04Z starting" || got[1] != "2026-10-15 21:50:04Z error" || got[2] != "no timestamp" {
		t.Fatalf("%q", got)
	}
}

func TestContainersPortsAndIPs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"abc","Names":["/web"],"Image":"nginx:1.27","State":"running","Status":"Up 2 hours",
			"Labels":{"com.docker.compose.project":"site"},
			"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"IP":"::","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"PrivatePort":9000,"Type":"tcp"}],
			"NetworkSettings":{"Networks":{"site_default":{"IPAddress":"172.18.0.3"},"bridge":{"IPAddress":"172.17.0.2"}}}}]`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	list, err := NewClient(sock).Containers(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	c := list[0]
	if len(c.Ports) != 1 || c.Ports[0] != (report.PortMap{Public: 8080, Private: 80, Proto: "tcp"}) {
		t.Errorf("ports = %v (want 8080->80 once; unpublished 9000 left out)", c.Ports)
	}
	if len(c.IPs) != 2 || c.IPs[0] != "172.17.0.2" || c.IPs[1] != "172.18.0.3" {
		t.Errorf("ips = %v", c.IPs)
	}
}
