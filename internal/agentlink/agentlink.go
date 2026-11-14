// Package agentlink is the agent's side of its connection to obs-beacond:
// which HTTP client to use, how to prove who it is, and keeping its
// certificate.
//
// Over https, the agent pins obs-beacond's agent CA by fingerprint
// (internal/pki explains why there's no name check). It enrolls once with
// its token, gets a client certificate, keeps it and its key in its state
// directory, and renews it once two thirds of its life has passed. Reports
// carry only the certificate.
//
// Over plain http (an old server, or agent_tls.off inside a VPN), every
// request carries the token, as before.
package agentlink

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/pki"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Config is the part of agent.yaml the link uses.
type Config struct {
	Server        string // https://host:8461, or http:// for a plain listener
	CAFingerprint string // required with https
	TokenFile     string // enrolls (https) or signs every request (http)
	StateDir      string // where the certificate and key live (https), unless Store is set
	Store         Store  // where they live instead: the cluster agent's Secret
}

// Store keeps the agent's key, certificate and CA certificate (PEM).
type Store interface {
	// Load returns what was kept, or an error wrapping os.ErrNotExist
	// when nothing was.
	Load() (key, cert, ca []byte, err error)
	Save(key, cert, ca []byte) error
}

// dirStore keeps them as files in the state directory.
type dirStore string

func (d dirStore) Load() (key, cert, ca []byte, err error) {
	read := func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(string(d), n)) }
	if key, err = read(keyFile); err != nil {
		return
	}
	if cert, err = read(certFile); err != nil {
		return
	}
	ca, err = read(caFile)
	return
}

// Save replaces each file whole; if the agent stops between two of them,
// Load finds a pair that doesn't match and the agent enrolls again.
func (d dirStore) Save(key, cert, ca []byte) error {
	for _, f := range []struct {
		name string
		b    []byte
	}{{keyFile, key}, {certFile, cert}, {caFile, ca}} {
		if err := writeAtomic(filepath.Join(string(d), f.name), f.b); err != nil {
			return err
		}
	}
	return nil
}

// File names in StateDir.
const (
	keyFile  = "agent.key"
	certFile = "agent.crt"
	caFile   = "ca.pem"
)

// Link is the agent's connection to obs-beacond.
type Link struct {
	cfg    Config
	store  Store
	tlsOn  bool
	pin    string
	client *http.Client

	mu    sync.Mutex
	cert  *tls.Certificate // nil until enrolled
	token string           // read when needed; "" when there is no token file
	note  string           // the last problem logged, so it's logged once
}

// New sets up the link. Over https it loads a certificate kept from before;
// it doesn't talk to obs-beacond yet.
func New(cfg Config) (*Link, error) {
	u, err := url.Parse(cfg.Server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("server %q: want https://host:port (or http:// for a plain listener)", cfg.Server)
	}
	l := &Link{cfg: cfg, tlsOn: u.Scheme == "https"}
	if !l.tlsOn {
		if cfg.TokenFile == "" {
			return nil, errors.New("a plain http server needs token_file")
		}
		if l.token, err = readToken(cfg.TokenFile); err != nil {
			return nil, err
		}
		l.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, IdleConnTimeout: time.Minute}}
		return l, nil
	}
	if cfg.CAFingerprint == "" {
		return nil, errors.New("an https server needs ca_fingerprint (`obs-beacond ca` on the server prints it)")
	}
	if l.pin, err = pki.NormalizeFingerprint(cfg.CAFingerprint); err != nil {
		return nil, fmt.Errorf("ca_fingerprint: %w", err)
	}
	l.store = cfg.Store
	if l.store == nil {
		if cfg.StateDir == "" {
			return nil, errors.New("an https server needs state_dir")
		}
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return nil, err
		}
		l.store = dirStore(cfg.StateDir)
	}
	if c, err := l.load(); err == nil {
		l.cert = c
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("the kept certificate can't be used, enrolling again: %v", err)
	}
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The chain is checked below against the pinned CA, without a name
		// check (internal/pki says why); Go's own check would want a name.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := pki.VerifyPinned(raw, l.pin, time.Now())
			return err
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.cert == nil {
				return &tls.Certificate{}, nil // none yet: enrolling
			}
			return l.cert, nil
		},
	}
	l.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: tc, MaxIdleConns: 2, IdleConnTimeout: time.Minute,
		ForceAttemptHTTP2: true,
	}}
	return l, nil
}

// Client is the HTTP client for every request to obs-beacond.
func (l *Link) Client() *http.Client { return l.client }

// TLS says whether this is an https link.
func (l *Link) TLS() bool { return l.tlsOn }

// Authorize adds what a request needs to prove the agent: the token over
// http, nothing over https (the certificate does it).
func (l *Link) Authorize(req *http.Request) {
	if !l.tlsOn {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}
}

// Ready says whether requests can be sent: over https, once enrolled.
func (l *Link) Ready() bool {
	if !l.tlsOn {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cert != nil
}

// Name is the agent name in the certificate; "" before enrolling.
func (l *Link) Name() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cert == nil {
		return ""
	}
	return l.cert.Leaf.Subject.CommonName
}

// NotAfter is when the certificate runs out; zero before enrolling.
func (l *Link) NotAfter() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cert == nil {
		return time.Time{}
	}
	return l.cert.Leaf.NotAfter
}

// Maintain enrolls when there's no certificate, and renews one past two
// thirds of its life (falling back to enrolling when it has run out). Call
// it before every report; it does nothing most of the time.
func (l *Link) Maintain(ctx context.Context, now time.Time) error {
	if !l.tlsOn {
		return nil
	}
	l.mu.Lock()
	cert := l.cert
	l.mu.Unlock()
	switch {
	case cert == nil:
		return l.logged(l.enroll(ctx))
	case now.After(cert.Leaf.NotAfter):
		log.Printf("the agent certificate ran out on %s; enrolling again", cert.Leaf.NotAfter.UTC().Format(time.DateOnly))
		l.drop()
		return l.logged(l.enroll(ctx))
	case now.After(renewAt(cert.Leaf)):
		if err := l.renew(ctx); err != nil {
			return l.logged(fmt.Errorf("renewing (the certificate is good until %s): %w", cert.Leaf.NotAfter.UTC().Format(time.DateOnly), err))
		}
		l.logged(nil)
	}
	return nil
}

// Refused is called when obs-beacond answers 401 to a request carrying the
// certificate: it was revoked (the agent enrolled elsewhere, or obs-beacond's
// database was restored from before it). With a token, the next Maintain
// enrolls again; without one, the certificate is all the agent has.
func (l *Link) Refused() {
	if !l.tlsOn || l.cfg.TokenFile == "" {
		return
	}
	if _, err := os.Stat(l.cfg.TokenFile); err != nil {
		return
	}
	log.Printf("obs-beacond refuses this agent's certificate; enrolling again with the token")
	l.drop()
}

func (l *Link) drop() {
	l.mu.Lock()
	l.cert = nil
	l.mu.Unlock()
	l.client.CloseIdleConnections()
}

// logged logs err once (and when it changes), and notes recovery.
func (l *Link) logged(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case err == nil && l.note != "":
		l.note = ""
	case err != nil && err.Error() != l.note:
		l.note = err.Error()
		log.Printf("agent certificate: %v", err)
	}
	return err
}

func renewAt(c *x509.Certificate) time.Time {
	life := c.NotAfter.Sub(c.NotBefore)
	return c.NotBefore.Add(life * 2 / 3)
}

func (l *Link) enroll(ctx context.Context) error {
	if l.cfg.TokenFile == "" {
		return errors.New("no certificate yet, and no token_file to enroll with")
	}
	tok, err := readToken(l.cfg.TokenFile)
	if err != nil {
		return err
	}
	if _, err := l.issue(ctx, "/api/v1/agent/enroll", tok); err != nil {
		return fmt.Errorf("enrolling: %w", err)
	}
	log.Printf("enrolled: agent certificate valid until %s", l.NotAfter().UTC().Format(time.DateOnly))
	return nil
}

func (l *Link) renew(ctx context.Context) error {
	if _, err := l.issue(ctx, "/api/v1/agent/renew", ""); err != nil {
		return err
	}
	log.Printf("renewed: agent certificate valid until %s", l.NotAfter().UTC().Format(time.DateOnly))
	return nil
}

// issue sends a new key's request to enroll, renew or join and keeps the
// result. It returns the agent name obs-beacond gave, when it gave one
// (join does).
func (l *Link) issue(ctx context.Context, path, token string) (string, error) {
	keyPEM, csrPEM, err := pki.NewKey()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.cfg.Server, "/")+path, bytes.NewReader(csrPEM))
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/pkcs10")
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var got struct {
		Cert  string `json:"cert"`
		CA    string `json:"ca"`
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&got); err != nil {
		return "", fmt.Errorf("answer: %w", err)
	}
	pair, err := l.check([]byte(got.Cert), keyPEM, []byte(got.CA))
	if err != nil {
		return "", err
	}
	if err := l.save(keyPEM, []byte(got.Cert), []byte(got.CA)); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.cert = pair
	l.mu.Unlock()
	l.client.CloseIdleConnections() // new handshakes present the new certificate
	return pair.Leaf.Subject.CommonName, nil
}

// Join spends a join token (internal/jointoken): it sends a new key's
// request with the token's secret, keeps the certificate in stateDir, and
// returns the agent name obs-beacond gave this host.
func Join(ctx context.Context, server, caFingerprint, secret, stateDir string) (string, error) {
	return JoinStore(ctx, Config{Server: server, CAFingerprint: caFingerprint, StateDir: stateDir}, secret)
}

// JoinStore is Join keeping the certificate where cfg says (cfg.Store, or
// cfg.StateDir).
func JoinStore(ctx context.Context, cfg Config, secret string) (string, error) {
	l, err := New(cfg)
	if err != nil {
		return "", err
	}
	if !l.tlsOn {
		return "", errors.New("joining needs an https server")
	}
	name, err := l.issue(ctx, "/api/v1/agent/join", secret)
	if err != nil {
		return "", fmt.Errorf("joining: %w", err)
	}
	return name, nil
}

// check makes sure a certificate obs-beacond sent is for our key, from the
// pinned CA, and for client use.
func (l *Link) check(certPEM, keyPEM, caPEM []byte) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the certificate doesn't match the key: %w", err)
	}
	blk, _ := pem.Decode(caPEM)
	if blk == nil {
		return nil, errors.New("no CA certificate in the answer")
	}
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, err
	}
	if pki.Fingerprint(ca) != l.pin {
		return nil, errors.New("the answer's CA is not the pinned one")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	return &pair, nil
}

// load reads the kept certificate and key, checked as when they arrived.
func (l *Link) load() (*tls.Certificate, error) {
	key, cert, ca, err := l.store.Load()
	if err != nil {
		return nil, err
	}
	return l.check(cert, key, ca)
}

// save keeps the key, certificate and CA.
func (l *Link) save(key, cert, ca []byte) error { return l.store.Save(key, cert, ca) }

func writeAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p) // CreateTemp makes it 0600
}

func readToken(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return t, nil
}
