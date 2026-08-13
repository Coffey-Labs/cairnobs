package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/sentry/sentry/ingest/internal/config"
)

// loadServerTLSConfig builds the mTLS server config: ingest's own
// certificate, plus the CA used to verify agent client certificates.
// Agents are never accepted without a client cert signed by this CA.
func loadServerTLSConfig(cfg config.TLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server cert/key: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading client CA file: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in client CA file %s", cfg.ClientCAFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
