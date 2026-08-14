// Package config loads ingest's configuration from environment variables.
// Phase 0 deliberately has no config file format of its own — env vars are
// enough for a docker-compose/k8s deployment and avoid pulling in a config
// library.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	GRPC       GRPCConfig
	TLS        TLSConfig
	Redpanda   RedpandaConfig
	ClickHouse ClickHouseConfig
	Batch      BatchConfig
	// EnterpriseAuthURL enables per-tenant ingest credential validation
	// (internal/grpcserver.TenantResolver) when set -- empty (the
	// default) is a documented no-op, same "off unless configured" shape
	// as every other optional enterprise integration point in this
	// codebase (e.g. api's own ENTERPRISE_AUTH_URL).
	EnterpriseAuthURL string
}

type GRPCConfig struct {
	ListenAddr string
}

// TLSConfig is the server-side mTLS material: the ingest service's own
// cert/key, and the CA used to verify agent client certs.
type TLSConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

type RedpandaConfig struct {
	Brokers       []string
	Topic         string
	ConsumerGroup string
}

type ClickHouseConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

type BatchConfig struct {
	MaxSize         int
	FlushIntervalMS int
}

func Load() (Config, error) {
	cfg := Config{
		GRPC: GRPCConfig{
			ListenAddr: getenv("GRPC_LISTEN_ADDR", ":4317"),
		},
		TLS: TLSConfig{
			CertFile:     getenv("TLS_CERT_FILE", "/etc/sentry-ingest/server.pem"),
			KeyFile:      getenv("TLS_KEY_FILE", "/etc/sentry-ingest/server-key.pem"),
			ClientCAFile: getenv("TLS_CLIENT_CA_FILE", "/etc/sentry-ingest/ca.pem"),
		},
		Redpanda: RedpandaConfig{
			Brokers:       strings.Split(getenv("REDPANDA_BROKERS", "localhost:9092"), ","),
			Topic:         getenv("REDPANDA_TOPIC", "sentry.logs.raw"),
			ConsumerGroup: getenv("REDPANDA_CONSUMER_GROUP", "sentry-ingest"),
		},
		ClickHouse: ClickHouseConfig{
			Addr:     getenv("CLICKHOUSE_ADDR", "localhost:9000"),
			Database: getenv("CLICKHOUSE_DATABASE", "sentry"),
			Username: getenv("CLICKHOUSE_USERNAME", "default"),
			Password: getenv("CLICKHOUSE_PASSWORD", ""),
		},
		EnterpriseAuthURL: getenv("ENTERPRISE_AUTH_URL", ""),
	}

	maxSize, err := strconv.Atoi(getenv("CONSUMER_BATCH_MAX_SIZE", "500"))
	if err != nil {
		return Config{}, fmt.Errorf("CONSUMER_BATCH_MAX_SIZE: %w", err)
	}
	cfg.Batch.MaxSize = maxSize

	flushMS, err := strconv.Atoi(getenv("CONSUMER_BATCH_FLUSH_INTERVAL_MS", "2000"))
	if err != nil {
		return Config{}, fmt.Errorf("CONSUMER_BATCH_FLUSH_INTERVAL_MS: %w", err)
	}
	cfg.Batch.FlushIntervalMS = flushMS

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
