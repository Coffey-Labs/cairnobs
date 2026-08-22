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
	// AgentRegistry enables agent inventory/remote config
	// (internal/agentregistry, internal/grpcserver.AgentRegistry) when
	// Postgres.Addr is set -- same "off unless configured" shape as
	// EnterpriseAuthURL above. Writes into the same sentry_metadata
	// database api/web already use, via the same shared "sentry" role
	// every other non-audit table in this schema uses (unlike
	// audit_log's dedicated restricted role -- agent inventory carries
	// no tamper-evidence requirement).
	AgentRegistry AgentRegistryConfig
}

type AgentRegistryConfig struct {
	Postgres PostgresConfig
}

type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
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

// devOnlyCredential is docker-compose.yml's zero-config default for
// every Postgres/ClickHouse password in this repo -- see
// api/internal/config.Config.DevCredentialWarnings for the full
// reasoning (duplicated here per this repo's no-shared-code-between-
// services convention).
const devOnlyCredential = "cairnobs-dev-only"

// DevCredentialWarnings reports which configured credentials still
// equal the literal dev-only default -- cmd/ingest/main.go logs each
// one loudly at startup. A warning, not a startup-refusing error: local
// dev's zero-config docker-compose.yml path legitimately leaves every
// password at this value.
func (c Config) DevCredentialWarnings() []string {
	var warnings []string
	if c.ClickHouse.Password == devOnlyCredential {
		warnings = append(warnings, "CLICKHOUSE_PASSWORD is still the default dev-only value -- set a real password before this is reachable outside local dev")
	}
	if c.AgentRegistry.Postgres.Password == devOnlyCredential {
		warnings = append(warnings, "AGENT_REGISTRY_POSTGRES_PASSWORD is still the default dev-only value -- set a real password before this is reachable outside local dev")
	}
	return warnings
}

func Load() (Config, error) {
	cfg := Config{
		GRPC: GRPCConfig{
			ListenAddr: getenv("GRPC_LISTEN_ADDR", ":4317"),
		},
		TLS: TLSConfig{
			CertFile:     getenv("TLS_CERT_FILE", "/etc/cairnobs-ingest/server.pem"),
			KeyFile:      getenv("TLS_KEY_FILE", "/etc/cairnobs-ingest/server-key.pem"),
			ClientCAFile: getenv("TLS_CLIENT_CA_FILE", "/etc/cairnobs-ingest/ca.pem"),
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
		AgentRegistry: AgentRegistryConfig{
			Postgres: PostgresConfig{
				Addr:     getenv("AGENT_REGISTRY_POSTGRES_ADDR", ""),
				Database: getenv("AGENT_REGISTRY_POSTGRES_DATABASE", "sentry_metadata"),
				Username: getenv("AGENT_REGISTRY_POSTGRES_USERNAME", "sentry"),
				Password: getenv("AGENT_REGISTRY_POSTGRES_PASSWORD", ""),
			},
		},
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
