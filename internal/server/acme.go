package server

// Built-in ACME: with a real host name,
// obs-beacond gets and renews its own certificate for `listen`, so passkeys
// and secure cookies work without a proxy in front. Challenges are
// TLS-ALPN-01 on `listen` (which must be what port 443 reaches) and, with
// `http_listen` (port 80), HTTP-01; that listener sends everything else to
// https.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// ACMEConfig is tls.acme in obs-beacond.yaml.
type ACMEConfig struct {
	Hostnames   []string `yaml:"hostnames"`    // the names on the certificate; the first is the public address
	Email       string   `yaml:"email"`        // for the CA's expiry and problem notices; optional
	AcceptTerms bool     `yaml:"accept_terms"` // the CA's subscriber agreement; required
	Directory   string   `yaml:"directory"`    // default Let's Encrypt
	CAFile      string   `yaml:"ca_file"`      // trust for a private ACME server's own HTTPS (step-ca and the like)
	HTTPListen  string   `yaml:"http_listen"`  // e.g. ":80": HTTP-01 and a redirect to https
	Cache       string   `yaml:"cache"`        // account key and certificates; default acme/ next to the database
}

// letsEncrypt is the default ACME directory.
const letsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

func (c *ACMEConfig) check(cfg *Config) error {
	if cfg.TLS.Cert != "" || cfg.TLS.Key != "" {
		return errors.New("tls: cert/key and acme are either-or")
	}
	if len(c.Hostnames) == 0 {
		return errors.New("tls.acme: hostnames is required")
	}
	for i, h := range c.Hostnames {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h == "" || strings.ContainsAny(h, " /:*") || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
			return fmt.Errorf("tls.acme: %q is not a host name a public CA can certify", c.Hostnames[i])
		}
		c.Hostnames[i] = h
	}
	if c.Directory == "" {
		c.Directory = letsEncrypt
	}
	if !c.AcceptTerms {
		return fmt.Errorf("tls.acme: set accept_terms: true to agree to the subscriber agreement of %s (Let's Encrypt's is at https://letsencrypt.org/repository/)", c.Directory)
	}
	if c.Cache == "" {
		if cfg.Dir() == "" {
			return errors.New("tls.acme: set cache (a directory for the account key and certificates), or database")
		}
		c.Cache = filepath.Join(cfg.Dir(), "acme")
	}
	return nil
}

// publicFromACME is the public address the certificate's first name makes:
// https://name, with listen's port unless it's 443.
func publicFromACME(c *Config) string {
	u := "https://" + c.TLS.ACME.Hostnames[0]
	if _, port, err := net.SplitHostPort(c.Listen); err == nil && port != "443" && port != "" {
		u += ":" + port
	}
	return u
}

func newACME(c *ACMEConfig) (*autocert.Manager, error) {
	if err := os.MkdirAll(c.Cache, 0o700); err != nil {
		return nil, fmt.Errorf("tls.acme cache: %w", err)
	}
	hc := http.DefaultClient
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls.acme ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.acme ca_file %s: no certificates", c.CAFile)
		}
		hc = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(c.Cache),
		HostPolicy: autocert.HostWhitelist(c.Hostnames...),
		Email:      c.Email,
		Client:     &acme.Client{DirectoryURL: c.Directory, HTTPClient: hc},
	}, nil
}

// webTLSConfig is listen's TLS config with ACME.
func (srv *Server) webTLSConfig() *tls.Config {
	t := srv.acme.TLSConfig()
	t.MinVersion = tls.VersionTLS12
	return t
}

// warmACME gets the certificates as soon as the listener is up, so the first
// visitor doesn't wait and a problem shows in the log at once rather than
// at the first visit. autocert renews them from then on.
func (srv *Server) warmACME(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second): // the listener answers TLS-ALPN-01
	}
	for _, h := range srv.cfg.TLS.ACME.Hostnames {
		hello := &tls.ClientHelloInfo{
			ServerName:       h,
			SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			SupportedCurves:  []tls.CurveID{tls.CurveP256},
			CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		}
		cert, err := srv.acme.GetCertificate(hello)
		if err != nil {
			log.Printf("tls.acme: no certificate for %s yet: %v (the listener must be what port 443 of %s reaches, or http_listen port 80)", h, err, h)
			continue
		}
		log.Printf("tls.acme: certificate for %s, valid until %s", h, cert.Leaf.NotAfter.UTC().Format(time.DateOnly))
	}
}

// acmeHTTPServer answers HTTP-01 on http_listen and sends the rest to https.
func (srv *Server) acmeHTTPHandler() http.Handler {
	pub := srv.cfg.Alerts.PublicURL
	return srv.acme.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "use https", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, pub+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
}
