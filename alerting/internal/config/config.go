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
	APIServiceToken   string // RoleService credential presented to /api's POST /query -- see queryclient.New's doc comment
	CORSAllowedOrigin string
	Evaluator         EvaluatorConfig
	// LocalAuthEnabled gates alerting's own session-required middleware
	// (see internal/sessioncheck) -- same env var name as api's
	// LOCAL_AUTH_ENABLED, one consistent on/off switch across both
	// services for a single-tenant deployment turning local login on.
	LocalAuthEnabled bool
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

// devOnlyCredential is docker-compose.yml's zero-config default for
// every Postgres/ClickHouse password in this repo -- see
// api/internal/config.Config.DevCredentialWarnings for the full
// reasoning (duplicated here per this repo's no-shared-code-between-
// services convention).
const devOnlyCredential = "cairnobs-dev-only"

// DevCredentialWarnings reports whether the configured Postgres
// credential still equals the literal dev-only default --
// cmd/alerting/main.go logs it loudly at startup. A warning, not a
// startup-refusing error: local dev's zero-config docker-compose.yml
// path legitimately leaves it at this value.
func (c Config) DevCredentialWarnings() []string {
	if c.Postgres.Password == devOnlyCredential {
		return []string{"POSTGRES_PASSWORD is still the default dev-only value -- set a real password before this is reachable outside local dev"}
	}
	return nil
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
		// Empty by default -- matches Phase 0-3 behavior for a
		// single-tenant deployment with no enterprise/ deployed (api's
		// authorizer is nil there, so an absent token is fine).
		APIServiceToken: getenv("API_SERVICE_TOKEN", ""),
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

	localAuthEnabled, err := strconv.ParseBool(getenv("LOCAL_AUTH_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("LOCAL_AUTH_ENABLED: %w", err)
	}
	cfg.LocalAuthEnabled = localAuthEnabled

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
