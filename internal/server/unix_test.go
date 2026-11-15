package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listen: unix:PATH serves the web UI on a socket for a proxy on the same
// machine, replacing one an earlier run left; TLS on it is refused.
func TestUnixListen(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "obs.yaml")
	if err := os.WriteFile(cfg, []byte("listen: unix:"+dir+"/web.sock\ntls: {cert: /x, key: /y}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "unix socket") {
		t.Errorf("TLS on a unix socket: %v", err)
	}

	sock := filepath.Join(dir, "web.sock")
	for range 2 { // the second time, over the first's leftover socket
		l, err := listenUnix(sock)
		if err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o666 {
			t.Fatalf("socket mode: %v %v", fi.Mode(), err)
		}
		hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello") })}
		go hs.Serve(l)
		c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		res, err := c.Get("http://obs/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != "hello" {
			t.Errorf("got %q", b)
		}
		// Closing the listener removes the socket file; put one back, as a
		// killed process would leave it.
		hs.Close()
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			l2, _ := net.Listen("unix", sock)
			l2.(*net.UnixListener).SetUnlinkOnClose(false)
			l2.Close()
		}
	}
	// Something that isn't a socket is never removed.
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, nil, 0o600)
	if _, err := listenUnix(plain); err == nil {
		t.Error("listened over a plain file")
	}
}
