package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/certreload"
	"github.com/cairnobs/cairnobs/internal/docker"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/tlsscan"
)

func testCert(t *testing.T, name string, end time.Time) (tls.Certificate, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: end}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

// The agent asks the helper only for a unit in its cert_reload, on a port
// the host listens on, and says done only once the port serves the new
// certificate.
func TestReloadAction(t *testing.T) {
	oldCert, oldFP := testCert(t, "web.example", time.Now().Add(48*time.Hour))
	newCert, newFP := testCert(t, "web.example", time.Now().Add(90*24*time.Hour))
	var served atomic.Pointer[tls.Certificate]
	served.Store(&oldCert)
	// No SNI is sent to an address, so GetCertificate is the one place.
	tl, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return served.Load(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
		}
	}()
	_, ps, _ := net.SplitHostPort(tl.Addr().String())
	port, _ := strconv.Atoi(ps)

	// The helper: reloads by serving the new certificate, unless told it
	// "forgets".
	var asked atomic.Int32
	var forget atomic.Bool
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+docker.ReloadPath, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		var req certreload.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Unit != "nginx.service" || req.Served != oldFP || len(req.Names) != 1 || req.Names[0] != "web.example" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unexpected request"})
			return
		}
		if !forget.Load() {
			served.Store(&newCert)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": certreload.Result{File: "/etc/ssl/web.pem", Fingerprint: newFP, Ends: time.Now().Add(90 * 24 * time.Hour)}})
	})
	hs := &http.Server{Handler: mux}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Close()

	scan := tlsscan.New("web1")
	scan.Update([]report.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: port}}, time.Now())
	a := &agent{cfg: &config{CertReload: []report.ReloadRule{{Unit: "nginx.service", Files: []string{"/etc/ssl/*.pem"}}}},
		docker: docker.NewClient(sock), certs: scan}
	args := func(unit string, port int) json.RawMessage {
		b, _ := json.Marshal(reloadArgs{Unit: unit, Addr: "127.0.0.1", Port: port})
		return b
	}
	if len(a.reloadRules()) != 1 {
		t.Fatal("rules not offered")
	}

	if _, err := a.reload(t.Context(), args("sshd.service", port)); err == nil || !strings.Contains(err.Error(), "isn't in this host's cert_reload") {
		t.Fatalf("unlisted: %v", err)
	}
	if _, err := a.reload(t.Context(), args("nginx.service", port+1)); err == nil || !strings.Contains(err.Error(), "doesn't listen on") {
		t.Fatalf("other port: %v", err)
	}
	if asked.Load() != 0 {
		t.Fatal("helper asked for a refused reload")
	}

	confirmFor = 1500 * time.Millisecond
	defer func() { confirmFor = 15 * time.Second }()
	forget.Store(true)
	if _, err := a.reload(t.Context(), args("nginx.service", port)); err == nil || !strings.Contains(err.Error(), "still serves") {
		t.Fatalf("not reloaded: %v", err)
	}
	forget.Store(false)
	detail, err := a.reload(t.Context(), args("nginx.service", port))
	if err != nil || !strings.Contains(detail, "now serves /etc/ssl/web.pem") {
		t.Fatalf("reload: %q %v", detail, err)
	}
	// The next report carries what the port serves now.
	if r := scan.Report(time.Now()); r == nil || len(r.List) != 1 || r.List[0].Cert.Fingerprint != newFP {
		t.Fatalf("report: %+v", r)
	}
}
