// Command enterprise-api is the multi-tenant-aware alternative to
// api/cmd/api -- same POST /query and /dashboards surface (it reuses
// api/queryapi and api/dashboards's actual Handler types unchanged), but
// backed by a per-tenant ClickHouse connection registry
// (enterprise/internal/chrunner), a per-tenant Tantivy search client
// (enterprise/internal/searchclient), and a real audit logger
// (enterprise/internal/audit.QueryAPILogger) instead of the single
// shared connections and nil audit logger api/cmd/api's binary carries.
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
// Both Helm (deploy/helm/sentry/templates/api.yaml vs
// enterprise-api.yaml) and docker-compose.yml (COMPOSE_PROFILES) now
// make this the deployment-topology choice, not just a binary sitting
// unused alongside api's -- see this repo's CLAUDE.md. `search`'s write
// side (ingest, and by extension the Redpanda consumer search itself
// runs) is still not tenant-aware -- see enterprise/internal/searchclient
// and search/src/registry.rs's doc comments, and
// /docs/security/threat-model.md.
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

	"github.com/sentry/sentry/enterprise/internal/apiconfig"
	"github.com/sentry/sentry/enterprise/internal/audit"
	"github.com/sentry/sentry/enterprise/internal/chrunner"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/enterprise/internal/searchclient"
	"github.com/sentry/sentry/enterprise/internal/tenantcrd"
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

	search, err := searchclient.Dial(cfg.SearchGRPCAddr, rbac)
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
	dashboardsHandler := dashboards.NewHandler(logger, dashboards.NewStore(pgPool), authorizer, rbacstore.NewDashboardPermissions(rbac))

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
//
// If cfg.TenantCRDNamespace is set, this also syncs the real result into
// the Tenant CRD (enterprise/internal/tenantcrd) -- the "lightweight
// unification" of this mechanism with deploy/operator's Tenant CRD (see
// that package's doc comment). An already-active tenant no longer
// refuses outright: ClickHouse (re-)provisioning is still refused (that
// part is unchanged -- rotating a live credential would break every
// open connection for no benefit), but CR sync alone is safe to retry
// using the credentials already on file in rbacstore, which matters if
// a previous run's CR sync failed (or TENANT_CRD_NAMESPACE is being
// turned on for a tenant provisioned before this feature existed) and
// needs to be retried without touching ClickHouse again.
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

	alreadyActive := tenant.Status == "active"
	if alreadyActive {
		logger.Info("tenant is already active -- skipping ClickHouse (re-)provisioning, will still sync the Tenant CRD if TENANT_CRD_NAMESPACE is set", "tenant_id", tenantID)
	}

	dataSource, err := rbac.GetDataSourceForTenant(ctx, tenantID)
	if err != nil {
		if err != rbacstore.ErrNotFound {
			logger.Error("getting data source", "error", err)
			return 1
		}
		if alreadyActive {
			// An active tenant with no data_sources row at all is
			// inconsistent state runProvisionTenant's own gate should
			// never have allowed -- fail loudly rather than silently
			// provisioning ClickHouse for a tenant already marked
			// active, which SetDataSourceClickHouseCredentials's own
			// doc comment says must never happen twice.
			logger.Error("tenant is active but has no data source row -- inconsistent state, refusing", "tenant_id", tenantID)
			return 1
		}
		dataSource, err = rbac.CreateDataSource(ctx, tenantID, "default", tenantID, "/var/lib/sentry-search/tenants/"+tenantID)
		if err != nil {
			logger.Error("creating data source", "error", err)
			return 1
		}
	}

	var creds tenantprovision.Credentials
	if alreadyActive {
		if dataSource.ClickHouseUsername == nil || dataSource.ClickHousePassword == nil {
			logger.Error("tenant is active but its data source has no ClickHouse credentials -- inconsistent state, refusing", "tenant_id", tenantID)
			return 1
		}
		creds = tenantprovision.Credentials{Username: *dataSource.ClickHouseUsername, Password: *dataSource.ClickHousePassword}
	} else {
		if dataSource.ClickHouseUsername != nil {
			logger.Error("data source already has ClickHouse credentials -- refusing to re-provision", "tenant_id", tenantID)
			return 1
		}
		creds, err = tenantprovision.New(adminConn).ProvisionClickHouse(ctx, tenantID)
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
	}

	if cfg.TenantCRDNamespace != "" {
		syncer, err := tenantcrd.New(cfg.TenantCRDNamespace)
		if err != nil {
			logger.Error("building tenant CRD syncer", "error", err)
			return 1
		}
		if err := syncer.Sync(ctx, tenantID, tenant.DisplayName, dataSource.TantivyIndexPath, tenantcrd.Credentials{Username: creds.Username, Password: creds.Password}); err != nil {
			logger.Error("syncing tenant CRD", "error", err)
			return 1
		}
		logger.Info("synced tenant CRD", "tenant_id", tenantID, "namespace", cfg.TenantCRDNamespace)
	}

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
