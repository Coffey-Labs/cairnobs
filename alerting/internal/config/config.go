// Package config loads alerting's configuration from environment
// variables, same convention as /api and /ingest: no config file format.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPListenAddr    string
	Postgres          PostgresConfig
	APIQueryURL       string // base URL of /api, e.g. http://api:8080 -- alerting never talks to ClickHouse/Tantivy directly
	CORSAllowedOrigin string
	Evaluator         EvaluatorConfig
}

type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

type EvaluatorConfig struct {
	TickInterval time.Duration // how often the scheduler checks for due rules
	// ClaimBatchSize and WorkerPoolSize are deliberately separate knobs,
	// not the same number: ClaimBatchSize bounds how many due rules one
	// tick pulls off the queue (needs to be large enough to drain a
	// backlog when many rules share a due time, e.g. right after bulk
	// creation), while WorkerPoolSize bounds concurrent /query calls
	// within that batch. Found by actually running hack/alert-load-test
	// with 500 rules and both numbers defaulted to the same value
	// (20): the evaluator took 125s to cycle through 500 due rules
	// instead of the configured 60s eval_interval_seconds, because each
	// 5s tick could only claim 20 rules regardless of how many more were
	// already due -- see /docs/phase-3-runbook.md's load-test section.
	ClaimBatchSize int
	WorkerPoolSize int
	QueryTimeout   time.Duration // per-evaluation POST /query timeout
}

func Load() (Config, error) {
	cfg := Config{
		HTTPListenAddr: getenv("HTTP_LISTEN_ADDR", ":8081"),
		Postgres: PostgresConfig{
			Addr:     getenv("POSTGRES_ADDR", "localhost:5432"),
			Database: getenv("POSTGRES_DATABASE", "sentry_metadata"),
			Username: getenv("POSTGRES_USERNAME", "sentry"),
			Password: getenv("POSTGRES_PASSWORD", ""),
		},
		APIQueryURL: getenv("API_QUERY_URL", "http://localhost:8080"),
		// Same "no auth yet" tradeoff as api's CORSAllowedOrigin default --
		// see api/internal/config/config.go's comment, same reasoning here.
		CORSAllowedOrigin: getenv("CORS_ALLOWED_ORIGIN", "*"),
	}

	tickSec, err := strconv.Atoi(getenv("EVALUATOR_TICK_SECONDS", "5"))
	if err != nil {
		return Config{}, fmt.Errorf("EVALUATOR_TICK_SECONDS: %w", err)
	}
	cfg.Evaluator.TickInterval = time.Duration(tickSec) * time.Second

	poolSize, err := strconv.Atoi(getenv("EVALUATOR_WORKER_POOL_SIZE", "20"))
	if err != nil {
		return Config{}, fmt.Errorf("EVALUATOR_WORKER_POOL_SIZE: %w", err)
	}
	cfg.Evaluator.WorkerPoolSize = poolSize

	// Default well above WorkerPoolSize: this is "how many due rules can
	// one tick pull off the queue," not a concurrency limit -- the
	// worker pool below still bounds actual concurrent /query calls
	// regardless of how large a batch gets claimed.
	claimBatchSize, err := strconv.Atoi(getenv("EVALUATOR_CLAIM_BATCH_SIZE", "1000"))
	if err != nil {
		return Config{}, fmt.Errorf("EVALUATOR_CLAIM_BATCH_SIZE: %w", err)
	}
	cfg.Evaluator.ClaimBatchSize = claimBatchSize

	queryTimeoutSec, err := strconv.Atoi(getenv("EVALUATOR_QUERY_TIMEOUT_SECONDS", "30"))
	if err != nil {
		return Config{}, fmt.Errorf("EVALUATOR_QUERY_TIMEOUT_SECONDS: %w", err)
	}
	cfg.Evaluator.QueryTimeout = time.Duration(queryTimeoutSec) * time.Second

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
