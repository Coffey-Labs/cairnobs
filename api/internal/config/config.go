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
	Postgres          PostgresConfig
	SearchGRPCAddr    string
	QueryTimeout      time.Duration
	CORSAllowedOrigin string
	EnterpriseAuthURL string
	AI                AIConfig
	LocalAuth         LocalAuthConfig
}

// LocalAuthConfig gates single-tenant mode's local username/password
// login (see api/localauth) -- off unless Enabled, same "off unless
// configured" convention as EnterpriseAuthURL/AI.OllamaBaseURL. Only
// meaningful when EnterpriseAuthURL is empty -- a deployment with real
// SSO configured has no use for a second, local auth mechanism, and
// main.go's authorizer selection treats EnterpriseAuthURL as taking
// priority if both were somehow set.
type LocalAuthConfig struct {
	Enabled bool
	// SessionTTL is deliberately long (30 days default) compared to
	// enterprise/'s session TTL -- there's no SSO round-trip here to
	// silently refresh a session against, so a short TTL would just mean
	// re-entering a password often on a self-hosted single-operator tool.
	SessionTTL time.Duration
	// CookieDomain empty means a host-only cookie (fine for local dev,
	// where web/api are both localhost:<port>). Set to e.g.
	// ".sentry.example.com" in production so the cookie is also sent to
	// api.sentry.example.com/alerting.sentry.example.com.
	CookieDomain string
	// CookieSecure defaults true (never sent over plain HTTP) --
	// deliberately opt-out via LOCAL_AUTH_COOKIE_SECURE=false, only
	// useful to test the login flow locally over http://localhost.
	CookieSecure bool
}

// AIConfig gates Phase 7's AI-assisted query features (Track A/B) --
// off unless OllamaBaseURL is set, same "off unless configured"
// convention as EnterpriseAuthURL and everything else optional in this
// codebase. OllamaFastModel is the per-operation override for
// Complete's tight latency budget (/docs/phase-7-ai-design.md's
// per-operation provider/model config) -- empty means Complete uses
// OllamaModel too, same as every other operation.
type AIConfig struct {
	OllamaBaseURL   string
	OllamaModel     string
	OllamaFastModel string
}

type ClickHouseConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

// PostgresConfig is the control-plane metadata store (dashboards, panels
// -- see /docs/phase-3-dashboard-design.md), distinct from ClickHouse
// which remains log-data-only.
type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

// devOnlyCredential is docker-compose.yml's zero-config default for
// every Postgres/ClickHouse password in this repo -- genuinely fine for
// local dev (that's the whole point of a zero-config default), but a
// real deployment that skips docker-compose.override.yml would
// otherwise go live with a password anyone can read straight off
// GitHub. See DevCredentialWarnings.
const devOnlyCredential = "sentry-dev-only"

// DevCredentialWarnings reports which configured credentials still
// equal docker-compose.yml's literal dev-only default -- cmd/api/main.go
// logs each one loudly at startup. Deliberately a warning, not a
// startup-refusing error: local dev's documented zero-config path is
// exactly "run docker-compose.yml with no override," which legitimately
// leaves every password at this literal value, so hard-failing here
// would break that path rather than only catching real deployments that
// forgot to override it.
func (c Config) DevCredentialWarnings() []string {
	var warnings []string
	if c.ClickHouse.Password == devOnlyCredential {
		warnings = append(warnings, "CLICKHOUSE_PASSWORD is still the default dev-only value -- set a real password via docker-compose.override.yml (or your deployment's equivalent) before this is reachable outside local dev")
	}
	if c.Postgres.Password == devOnlyCredential {
		warnings = append(warnings, "POSTGRES_PASSWORD is still the default dev-only value -- set a real password via docker-compose.override.yml (or your deployment's equivalent) before this is reachable outside local dev")
	}
	return warnings
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
		Postgres: PostgresConfig{
			Addr:     getenv("POSTGRES_ADDR", "localhost:5432"),
			Database: getenv("POSTGRES_DATABASE", "sentry_metadata"),
			Username: getenv("POSTGRES_USERNAME", "sentry"),
			Password: getenv("POSTGRES_PASSWORD", ""),
		},
		// Search service's gRPC address (see /search) -- default matches
		// /search's own default GRPC_LISTEN_ADDR.
		SearchGRPCAddr: getenv("SEARCH_GRPC_ADDR", "localhost:50052"),
		// Phase 0 has no auth, so this is wide open by default to keep
		// the local SvelteKit dev server (a different origin/port)
		// working out of the box. Tighten before this is ever reachable
		// from outside a trusted dev/homelab network.
		CORSAllowedOrigin: getenv("CORS_ALLOWED_ORIGIN", "*"),
		// Empty by default -- a single-tenant deployment without
		// enterprise/ configured runs with authz.RequireRole* as a
		// no-op, matching Phase 0-3 behavior. Set to enterprise-auth's
		// base URL (e.g. "http://enterprise-auth:8081") to turn on
		// real session/service-token enforcement.
		EnterpriseAuthURL: getenv("ENTERPRISE_AUTH_URL", ""),
		// Empty OllamaBaseURL means AI features are entirely disabled --
		// /ai/* routes aren't even registered (see main.go), matching
		// "no cloud dependency required for the default deployment" and,
		// by the same reasoning, no *local* model dependency forced on a
		// deployment that doesn't want one either. Model names default to
		// the recommendation confirmed in /docs/phase-7-ai-design.md.
		AI: AIConfig{
			OllamaBaseURL:   getenv("OLLAMA_BASE_URL", ""),
			OllamaModel:     getenv("OLLAMA_MODEL", "qwen2.5-coder:7b"),
			OllamaFastModel: getenv("OLLAMA_FAST_MODEL", "qwen2.5-coder:1.5b"),
		},
	}

	timeoutSec, err := strconv.Atoi(getenv("QUERY_TIMEOUT_SECONDS", "30"))
	if err != nil {
		return Config{}, fmt.Errorf("QUERY_TIMEOUT_SECONDS: %w", err)
	}
	cfg.QueryTimeout = time.Duration(timeoutSec) * time.Second

	localAuthEnabled, err := strconv.ParseBool(getenv("LOCAL_AUTH_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("LOCAL_AUTH_ENABLED: %w", err)
	}
	sessionTTLHours, err := strconv.Atoi(getenv("LOCAL_SESSION_TTL_HOURS", "720")) // 30 days
	if err != nil {
		return Config{}, fmt.Errorf("LOCAL_SESSION_TTL_HOURS: %w", err)
	}
	cookieSecure, err := strconv.ParseBool(getenv("LOCAL_AUTH_COOKIE_SECURE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("LOCAL_AUTH_COOKIE_SECURE: %w", err)
	}
	cfg.LocalAuth = LocalAuthConfig{
		Enabled:      localAuthEnabled,
		SessionTTL:   time.Duration(sessionTTLHours) * time.Hour,
		CookieDomain: getenv("SESSION_COOKIE_DOMAIN", ""),
		CookieSecure: cookieSecure,
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
