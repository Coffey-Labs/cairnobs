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
}

func TestLoadInvalidTimeoutErrors(t *testing.T) {
	t.Setenv("QUERY_TIMEOUT_SECONDS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric QUERY_TIMEOUT_SECONDS, got nil")
	}
}
