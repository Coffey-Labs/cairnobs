package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GRPC.ListenAddr != ":4317" {
		t.Errorf("GRPC.ListenAddr = %q, want :4317", cfg.GRPC.ListenAddr)
	}
	if cfg.Redpanda.Topic != "sentry.logs.raw" {
		t.Errorf("Redpanda.Topic = %q, want sentry.logs.raw", cfg.Redpanda.Topic)
	}
	if cfg.Batch.MaxSize != 500 {
		t.Errorf("Batch.MaxSize = %d, want 500", cfg.Batch.MaxSize)
	}
	if cfg.Batch.FlushIntervalMS != 2000 {
		t.Errorf("Batch.FlushIntervalMS = %d, want 2000", cfg.Batch.FlushIntervalMS)
	}
	if cfg.EnterpriseAuthURL != "" {
		t.Errorf("EnterpriseAuthURL = %q, want empty (tenant resolution off by default)", cfg.EnterpriseAuthURL)
	}
}

func TestLoadOverridesFromEnv(t *testing.T) {
	t.Setenv("GRPC_LISTEN_ADDR", ":9999")
	t.Setenv("REDPANDA_BROKERS", "a:9092,b:9092")
	t.Setenv("CONSUMER_BATCH_MAX_SIZE", "10")
	t.Setenv("ENTERPRISE_AUTH_URL", "http://enterprise-auth:8082")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GRPC.ListenAddr != ":9999" {
		t.Errorf("GRPC.ListenAddr = %q, want :9999", cfg.GRPC.ListenAddr)
	}
	if len(cfg.Redpanda.Brokers) != 2 || cfg.Redpanda.Brokers[0] != "a:9092" || cfg.Redpanda.Brokers[1] != "b:9092" {
		t.Errorf("Redpanda.Brokers = %+v, want [a:9092 b:9092]", cfg.Redpanda.Brokers)
	}
	if cfg.Batch.MaxSize != 10 {
		t.Errorf("Batch.MaxSize = %d, want 10", cfg.Batch.MaxSize)
	}
	if cfg.EnterpriseAuthURL != "http://enterprise-auth:8082" {
		t.Errorf("EnterpriseAuthURL = %q, want http://enterprise-auth:8082", cfg.EnterpriseAuthURL)
	}
}

func TestLoadInvalidBatchSizeErrors(t *testing.T) {
	t.Setenv("CONSUMER_BATCH_MAX_SIZE", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric CONSUMER_BATCH_MAX_SIZE, got nil")
	}
}
