// Package config loads api's configuration from environment variables,
// same convention as /ingest: no config file format for Phase 0.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPListenAddr    string
	ClickHouse        ClickHouseConfig
	SearchGRPCAddr    string
	QueryTimeout      time.Duration
	CORSAllowedOrigin string
}

type ClickHouseConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPListenAddr: getenv("HTTP_LISTEN_ADDR", ":8080"),
		ClickHouse: ClickHouseConfig{
			Addr:     getenv("CLICKHOUSE_ADDR", "localhost:9000"),
			Database: getenv("CLICKHOUSE_DATABASE", "sentry"),
			Username: getenv("CLICKHOUSE_USERNAME", "default"),
			Password: getenv("CLICKHOUSE_PASSWORD", ""),
		},
		// Search service's gRPC address (see /search) -- default matches
		// /search's own default GRPC_LISTEN_ADDR.
		SearchGRPCAddr: getenv("SEARCH_GRPC_ADDR", "localhost:50052"),
		// Phase 0 has no auth, so this is wide open by default to keep
		// the local SvelteKit dev server (a different origin/port)
		// working out of the box. Tighten before this is ever reachable
		// from outside a trusted dev/homelab network.
		CORSAllowedOrigin: getenv("CORS_ALLOWED_ORIGIN", "*"),
	}

	timeoutSec, err := strconv.Atoi(getenv("QUERY_TIMEOUT_SECONDS", "30"))
	if err != nil {
		return Config{}, fmt.Errorf("QUERY_TIMEOUT_SECONDS: %w", err)
	}
	cfg.QueryTimeout = time.Duration(timeoutSec) * time.Second

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
