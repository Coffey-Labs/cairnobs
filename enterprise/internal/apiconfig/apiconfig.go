// Package apiconfig loads enterprise-api's configuration from
// environment variables -- same convention as every other Go service in
// this repo. Named apiconfig, not config, to avoid colliding with the
// already-existing enterprise/internal/config (enterprise-auth's own,
// differently-shaped config) within the same module.
package apiconfig

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPListenAddr string
	// ClickHouseAddr is the shared physical ClickHouse server's native
	// address -- every tenant's connection (chrunner.Registry) and the
	// admin connection (tenantprovision) both dial this same address,
	// just with different credentials. Tenants sharing one physical
	// server is today's model; per-tenant dedicated cluster nodes is
	// named as later, non-schema-changing work in
	// /docs/phase-4-isolation-design.md.
	ClickHouseAddr string
	// ClickHouseAdmin is the access_management-enabled credential
	// tenantprovision uses for CREATE DATABASE/USER/GRANT -- the same
	// credential api's plain (non-enterprise) binary uses as its one
	// shared connection today (docker-compose.yml's CLICKHOUSE_PASSWORD).
	// Never used to run a tenant's actual queries.
	ClickHouseAdmin   ClickHouseAdminConfig
	Postgres          PostgresConfig
	AuditWriter       AuditWriterConfig
	SearchGRPCAddr    string
	QueryTimeout      time.Duration
	CORSAllowedOrigin string
	// EnterpriseAuthURL, like api's own config, is optional -- see that
	// package's doc comment on the nil-authorizer no-op default. In
	// practice a real enterprise-api deployment always sets this (there
	// is no reason to run this binary instead of plain api without RBAC
	// enforcement on), but nothing here hard-requires it, for the same
	// "never break a simpler deployment shape" reasoning used
	// throughout this codebase.
	EnterpriseAuthURL string
	// TenantCRDNamespace enables enterprise/internal/tenantcrd syncing
	// for -provision-tenant (cmd/enterprise-api/main.go's
	// runProvisionTenant) -- empty (the default) means skip it entirely,
	// same "off unless configured" shape as EnterpriseAuthURL above.
	// Deployments with no Kubernetes cluster at all (docker-compose)
	// never set this.
	TenantCRDNamespace string
}

type ClickHouseAdminConfig struct {
	Username string
	Password string
}

type PostgresConfig struct {
	Addr     string
	Database string
	Username string
	Password string
}

// AuditWriterConfig is the separate, narrowly-granted credential
// enterprise/internal/audit.Store requires -- see that package's doc
// comment on why it must never share api's/dashboards' pool.
type AuditWriterConfig struct {
	Username string
	Password string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPListenAddr: getenv("HTTP_LISTEN_ADDR", ":8083"),
		ClickHouseAddr: getenv("CLICKHOUSE_ADDR", "localhost:9000"),
		ClickHouseAdmin: ClickHouseAdminConfig{
			Username: getenv("CLICKHOUSE_ADMIN_USERNAME", "default"),
			Password: getenv("CLICKHOUSE_ADMIN_PASSWORD", ""),
		},
		Postgres: PostgresConfig{
			Addr:     getenv("POSTGRES_ADDR", "localhost:5432"),
			Database: getenv("POSTGRES_DATABASE", "sentry_metadata"),
			Username: getenv("POSTGRES_USERNAME", "sentry"),
			Password: getenv("POSTGRES_PASSWORD", ""),
		},
		AuditWriter: AuditWriterConfig{
			Username: getenv("AUDIT_WRITER_USERNAME", "audit_writer"),
			Password: getenv("AUDIT_WRITER_PASSWORD", ""),
		},
		SearchGRPCAddr:     getenv("SEARCH_GRPC_ADDR", "localhost:50052"),
		CORSAllowedOrigin:  getenv("CORS_ALLOWED_ORIGIN", "*"),
		EnterpriseAuthURL:  getenv("ENTERPRISE_AUTH_URL", ""),
		TenantCRDNamespace: getenv("TENANT_CRD_NAMESPACE", ""),
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
