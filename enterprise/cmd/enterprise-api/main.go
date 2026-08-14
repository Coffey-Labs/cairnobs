// Command enterprise-api is the multi-tenant-aware alternative to
// api/cmd/api -- same POST /query and /dashboards surface (it reuses
// api/queryapi and api/dashboards's actual Handler types unchanged), but
// backed by a per-tenant ClickHouse connection registry
// (enterprise/internal/chrunner) instead of the single shared connection
// api/cmd/api opens, and a real audit logger
// (enterprise/internal/audit.QueryAPILogger) instead of the nil api's
// binary has carried since Phase 4 task 4.
//
// Why a second binary, not a flag on api/cmd/api: api is AGPL core and
// must never import enterprise/ (hack/check-tenant-boundary.sh enforces
// this) -- there is no way for api's own binary to construct an
// enterprise-supplied chrunner.Registry or audit.Store without that
// import. enterprise/ importing api/ is the allowed direction, so this
// binary lives here instead, wiring core's handler types together with
// enterprise's tenant-aware implementations. A single-tenant deployment
// keeps running plain api/cmd/api, unchanged; a real multi-tenant
// deployment runs this one instead.
//
// Not built yet: per-tenant Tantivy routing (search stays the single
// shared api/searchclient.Dial connection every tenant shares --
// see /docs/security/threat-model.md), and the actual K8s/Helm wiring
// to run this binary in place of api's (docker-compose.yml adds it
// available, not defaulted into the traffic path, same shape as
// enterprise-auth's own addition in Phase 4 task 5).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/dashboards"
	"github.com/sentry/sentry/api/httpserver"
	"github.com/sentry/sentry/api/queryapi"
	"github.com/sentry/sentry/api/searchclient"

	"github.com/sentry/sentry/enterprise/internal/apiconfig"
	"github.com/sentry/sentry/enterprise/internal/audit"
	"github.com/sentry/sentry/enterprise/internal/chrunner"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/enterprise/internal/tenantprovision"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := apiconfig.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck(cfg.HTTPListenAddr))
	}

	provisionTenant := flag.String("provision-tenant", "", "provision ClickHouse for the named tenant id (creating it in rbacstore if needed) and exit")
	provisionDisplayName := flag.String("display-name", "", "display name for -provision-tenant, if the tenant doesn't already exist in rbacstore")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pgDSN := fmt.Sprintf("postgres://%s:%s@%s/%s", cfg.Postgres.Username, cfg.Postgres.Password, cfg.Postgres.Addr, cfg.Postgres.Database)
	pgPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		logger.Error("opening postgres pool", "error", err)
		os.Exit(1)
	}
	defer pgPool.Close()
	if err := pgPool.Ping(ctx); err != nil {
		logger.Error("pinging postgres", "error", err)
		os.Exit(1)
	}
	rbac := rbacstore.NewStore(pgPool)

	if *provisionTenant != "" {
		os.Exit(runProvisionTenant(ctx, logger, cfg, rbac, *provisionTenant, *provisionDisplayName))
	}

	adminConn, err := chdriver.Open(&chdriver.Options{
		Addr: []string{cfg.ClickHouseAddr},
		Auth: chdriver.Auth{Database: "default", Username: cfg.ClickHouseAdmin.Username, Password: cfg.ClickHouseAdmin.Password},
	})
	if err != nil {
		logger.Error("opening clickhouse admin connection", "error", err)
		os.Exit(1)
	}
	defer adminConn.Close()

	sources, err := rbac.ListProvisionedDataSources(ctx)
	if err != nil {
		logger.Error("listing provisioned data sources", "error", err)
		os.Exit(1)
	}
	chrunnerSources := make([]chrunner.DataSource, 0, len(sources))
	for _, s := range sources {
		if s.ClickHouseUsername == nil || s.ClickHousePassword == nil {
			continue // ListProvisionedDataSources already filters these out; defensive only.
		}
		chrunnerSources = append(chrunnerSources, chrunner.DataSource{
			TenantID: s.TenantID, Database: s.ClickHouseDatabaseName,
			Username: *s.ClickHouseUsername, Password: *s.ClickHousePassword,
		})
	}
	logger.Info("loaded tenant data sources", "count", len(chrunnerSources))

	registry, err := chrunner.New(ctx, cfg.ClickHouseAddr, chrunnerSources)
	if err != nil {
		logger.Error("building tenant connection registry", "error", err)
		os.Exit(1)
	}
	defer registry.Close()

	search, err := searchclient.Dial(cfg.SearchGRPCAddr)
	if err != nil {
		logger.Error("dialing search service", "error", err)
		os.Exit(1)
	}
	defer search.Close()

	var authorizer authz.Authorizer
	if cfg.EnterpriseAuthURL != "" {
		authorizer = authz.NewHTTPAuthorizer(cfg.EnterpriseAuthURL)
	} else {
		logger.Warn("ENTERPRISE_AUTH_URL is not set -- RBAC enforcement is a no-op, but tenant query routing still requires a resolved identity, so every /query request will be refused (see chrunner.Registry.RunSQL)")
	}

	auditWriterDSN := fmt.Sprintf("postgres://%s:%s@%s/%s", cfg.AuditWriter.Username, cfg.AuditWriter.Password, cfg.Postgres.Addr, cfg.Postgres.Database)
	auditPool, err := pgxpool.New(ctx, auditWriterDSN)
	if err != nil {
		logger.Error("opening audit_writer postgres pool", "error", err)
		os.Exit(1)
	}
	defer auditPool.Close()
	if err := auditPool.Ping(ctx); err != nil {
		logger.Error("pinging audit_writer postgres pool", "error", err)
		os.Exit(1)
	}
	auditLogger := audit.NewQueryAPILogger(audit.NewStore(auditPool), audit.SourceAPI)

	queryHandler := queryapi.NewHandler(logger, registry, search, cfg.QueryTimeout, auditLogger, authorizer)
	dashboardsHandler := dashboards.NewHandler(logger, dashboards.NewStore(pgPool), authorizer)

	mux := http.NewServeMux()
	queryHandler.RegisterRoutes(mux)
	dashboardsHandler.RegisterRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: httpserver.WithCORS(mux, cfg.CORSAllowedOrigin),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("enterprise-api listening", "addr", cfg.HTTPListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server exited with error", "error", err)
			os.Exit(1)
		}
	}
}

// runProvisionTenant is the operator action that actually closes
// /docs/phase-4-isolation-design.md's ordered provisioning gate: ensure
// the tenant row exists, ensure a data_sources row exists, provision
// ClickHouse (CREATE USER -> GRANT), persist the returned credentials,
// and only then mark the tenant active. Same "offline operator action,
// not a network-reachable endpoint" shape as enterprise-auth's
// -mint-service-token.
func runProvisionTenant(ctx context.Context, logger *slog.Logger, cfg apiconfig.Config, rbac *rbacstore.Store, tenantID, displayName string) int {
	adminConn, err := chdriver.Open(&chdriver.Options{
		Addr: []string{cfg.ClickHouseAddr},
		Auth: chdriver.Auth{Database: "default", Username: cfg.ClickHouseAdmin.Username, Password: cfg.ClickHouseAdmin.Password},
	})
	if err != nil {
		logger.Error("opening clickhouse admin connection", "error", err)
		return 1
	}
	defer adminConn.Close()

	tenant, err := rbac.GetTenant(ctx, tenantID)
	if err != nil {
		if err != rbacstore.ErrNotFound {
			logger.Error("getting tenant", "error", err)
			return 1
		}
		name := displayName
		if name == "" {
			name = tenantID
		}
		tenant, err = rbac.CreateTenant(ctx, tenantID, name)
		if err != nil {
			logger.Error("creating tenant", "error", err)
			return 1
		}
		logger.Info("created tenant row", "tenant_id", tenantID)
	}
	if tenant.Status == "active" {
		logger.Error("tenant is already active -- refusing to re-provision (would rotate a live credential)", "tenant_id", tenantID)
		return 1
	}

	dataSource, err := rbac.GetDataSourceForTenant(ctx, tenantID)
	if err != nil {
		if err != rbacstore.ErrNotFound {
			logger.Error("getting data source", "error", err)
			return 1
		}
		dataSource, err = rbac.CreateDataSource(ctx, tenantID, "default", tenantID, "/var/lib/sentry-search/tenants/"+tenantID)
		if err != nil {
			logger.Error("creating data source", "error", err)
			return 1
		}
	}
	if dataSource.ClickHouseUsername != nil {
		logger.Error("data source already has ClickHouse credentials -- refusing to re-provision", "tenant_id", tenantID)
		return 1
	}

	creds, err := tenantprovision.New(adminConn).ProvisionClickHouse(ctx, tenantID)
	if err != nil {
		logger.Error("provisioning clickhouse", "error", err)
		return 1
	}
	if err := rbac.SetDataSourceClickHouseCredentials(ctx, dataSource.ID, creds.Username, creds.Password); err != nil {
		logger.Error("persisting clickhouse credentials", "error", err)
		return 1
	}
	if err := rbac.SetTenantStatus(ctx, tenantID, "active"); err != nil {
		logger.Error("activating tenant", "error", err)
		return 1
	}

	logger.Info("tenant provisioned and active", "tenant_id", tenantID, "clickhouse_database", tenantID, "clickhouse_username", creds.Username)
	return 0
}

func runHealthcheck(listenAddr string) int {
	addr := listenAddr
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
