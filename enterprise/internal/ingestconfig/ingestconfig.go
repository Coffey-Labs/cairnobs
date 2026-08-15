// Package ingestconfig loads enterprise-ingest's configuration from
// environment variables -- same convention as every other Go service in
// this repo. Named ingestconfig, not config, to avoid colliding with
// enterprise/internal/config (enterprise-auth's own, differently-shaped
// config) within the same module -- mirrors enterprise/internal/
// apiconfig's own naming reasoning exactly.
package ingestconfig

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// HTTPListenAddr serves only /healthz -- this binary's actual job
	// (Redpanda -> per-tenant ClickHouse) has no other HTTP surface,
	// same "just enough for Docker's HEALTHCHECK" shape as every other
	// binary in this repo's -healthcheck self-check mode.
	HTTPListenAddr string
	// ClickHouseAddr is the shared physical ClickHouse server's native
	// address -- every tenant's connection (enterprise/internal/
	// chwriter.Registry) dials this same address, just with different
	// per-tenant credentials rbacstore already has on file from
	// enterprise-api -provision-tenant. Mirrors apiconfig.Config.
	// ClickHouseAddr's own doc comment.
	ClickHouseAddr string
	Postgres       PostgresConfig
	Redpanda       RedpandaConfig
	Batch          BatchConfig
}

type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

type RedpandaConfig struct {
	Brokers       []string
	Topic         string
	ConsumerGroup string
}

type BatchConfig struct {
	MaxSize         int
	FlushIntervalMS int
}

func Load() (Config, error) {
	cfg := Config{
		HTTPListenAddr: getenv("HTTP_LISTEN_ADDR", ":8084"),
		ClickHouseAddr: getenv("CLICKHOUSE_ADDR", "localhost:9000"),
		Postgres: PostgresConfig{
			Addr:     getenv("POSTGRES_ADDR", "localhost:5432"),
			Database: getenv("POSTGRES_DATABASE", "sentry_metadata"),
			Username: getenv("POSTGRES_USERNAME", "sentry"),
			Password: getenv("POSTGRES_PASSWORD", ""),
		},
		Redpanda: RedpandaConfig{
			Brokers: strings.Split(getenv("REDPANDA_BROKERS", "localhost:9092"), ","),
			// Same default topic ingest/internal/config uses -- this
			// binary reads the identical shared sentry.logs.raw topic
			// ingest/cmd/ingest's server half (agent-facing PushBatch)
			// produces onto; there's no per-tenant topic, see
			// ingest/internal/grpcserver's doc comment.
			Topic: getenv("REDPANDA_TOPIC", "sentry.logs.raw"),
			// A distinct consumer group from ingest/cmd/ingest's own
			// default ("sentry-ingest") -- this binary and a
			// single-tenant `ingest -mode=consumer` must never share a
			// group (each message would only ever reach one of them,
			// silently splitting traffic) even though in practice a
			// real multi-tenant deployment runs this binary *instead
			// of*, not alongside, `ingest -mode=consumer`.
			ConsumerGroup: getenv("REDPANDA_CONSUMER_GROUP", "sentry-enterprise-ingest"),
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
