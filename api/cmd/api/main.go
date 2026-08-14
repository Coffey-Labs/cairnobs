// Command api is Sentry's query API: a single POST /query endpoint
// accepting either the pipe syntax or raw SQL, compiled and routed
// across ClickHouse and search by internal/querylang. See
// internal/queryapi and /docs/query-language-design.md for why this is
// plain REST rather than the pinned gRPC+gateway pattern.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/internal/authz"
	"github.com/sentry/sentry/api/internal/config"
	"github.com/sentry/sentry/api/internal/dashboards"
	"github.com/sentry/sentry/api/internal/httpserver"
	"github.com/sentry/sentry/api/internal/queryapi"
	"github.com/sentry/sentry/api/internal/querylang/executor"
	"github.com/sentry/sentry/api/internal/searchclient"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	// -healthcheck: a self-check mode for Docker's HEALTHCHECK, not a
	// flag anyone runs by hand. The api image is distroless (no shell,
	// no wget/curl -- see api/Dockerfile), so docker-compose's
	// healthcheck execs this binary against itself instead of an
	// external tool. Exits before any ClickHouse/Postgres/search dial,
	// since those aren't what "is the HTTP server up" is asking.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck(cfg.HTTPListenAddr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.ClickHouse.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.ClickHouse.Database,
			Username: cfg.ClickHouse.Username,
			Password: cfg.ClickHouse.Password,
		},
	})
	if err != nil {
		logger.Error("opening clickhouse connection", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := conn.Ping(ctx); err != nil {
		logger.Error("pinging clickhouse", "error", err)
		os.Exit(1)
	}

	search, err := searchclient.Dial(cfg.SearchGRPCAddr)
	if err != nil {
		logger.Error("dialing search service", "error", err)
		os.Exit(1)
	}
	defer search.Close()

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

	// authorizer is nil (RequireRole* becomes a no-op) unless
	// ENTERPRISE_AUTH_URL is configured -- matches Phase 0-3 behavior
	// for a single-tenant deployment with no enterprise/ deployed.
	var authorizer authz.Authorizer
	if cfg.EnterpriseAuthURL != "" {
		authorizer = authz.NewHTTPAuthorizer(cfg.EnterpriseAuthURL)
	}

	sqlRunner := executor.NewChRunner(conn)
	// audit logging is nil (a no-op) until Phase 4 task 5 wires in
	// enterprise/internal/audit -- see queryapi.AuditLogger's doc comment.
	queryHandler := queryapi.NewHandler(logger, sqlRunner, search, cfg.QueryTimeout, nil, authorizer)
	dashboardsHandler := dashboards.NewHandler(logger, dashboards.NewStore(pgPool), authorizer)

	// One shared mux, CORS applied once around the whole thing -- see
	// internal/httpserver's doc comment for why this changed from each
	// handler wrapping itself individually.
	mux := http.NewServeMux()
	queryHandler.RegisterRoutes(mux)
	dashboardsHandler.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: httpserver.WithCORS(mux, cfg.CORSAllowedOrigin),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPListenAddr)
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

// runHealthcheck GETs its own /healthz and returns an exit code, for
// Docker's HEALTHCHECK to exec directly (see the -healthcheck flag
// above). listenAddr is HTTP_LISTEN_ADDR-shaped (e.g. ":8080") --
// "localhost" replaces a bare host part since that's this same
// container reaching itself, not another service.
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
