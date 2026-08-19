package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPListenAddr != ":8080" {
		t.Errorf("HTTPListenAddr = %q, want :8080", cfg.HTTPListenAddr)
	}
	if cfg.QueryTimeout != 30*time.Second {
		t.Errorf("QueryTimeout = %v, want 30s", cfg.QueryTimeout)
	}
	if cfg.CORSAllowedOrigin != "*" {
		t.Errorf("CORSAllowedOrigin = %q, want *", cfg.CORSAllowedOrigin)
	}
	if cfg.SearchGRPCAddr != "localhost:50052" {
		t.Errorf("SearchGRPCAddr = %q, want localhost:50052", cfg.SearchGRPCAddr)
	}
}

func TestLoadInvalidTimeoutErrors(t *testing.T) {
	t.Setenv("QUERY_TIMEOUT_SECONDS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric QUERY_TIMEOUT_SECONDS, got nil")
	}
}

// TestDevCredentialWarnings is the regression test for the
// security-audit finding that docker-compose.yml's hardcoded
// "sentry-dev-only" password has no runtime fail-safe if an operator
// forgets to override it for a real deployment.
func TestDevCredentialWarnings(t *testing.T) {
	if got := (Config{}).DevCredentialWarnings(); len(got) != 0 {
		t.Errorf("empty passwords: warnings = %v, want none", got)
	}

	real := Config{ClickHouse: ClickHouseConfig{Password: "a-real-password"}, Postgres: PostgresConfig{Password: "another-real-one"}}
	if got := real.DevCredentialWarnings(); len(got) != 0 {
		t.Errorf("real passwords: warnings = %v, want none", got)
	}

	devOnly := Config{ClickHouse: ClickHouseConfig{Password: devOnlyCredential}, Postgres: PostgresConfig{Password: devOnlyCredential}}
	if got := devOnly.DevCredentialWarnings(); len(got) != 2 {
		t.Errorf("both dev-default passwords: warnings = %v, want 2 entries", got)
	}
}
