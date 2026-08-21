// Command api is Sentry's query API: a single POST /query endpoint
// accepting either the pipe syntax or raw SQL, compiled and routed
// across ClickHouse and search by internal/querylang. See
// queryapi and /docs/query-language-design.md for why this is
// plain REST rather than the pinned gRPC+gateway pattern.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sentry/sentry/api/agents"
	"github.com/sentry/sentry/api/ai/aiapi"
	"github.com/sentry/sentry/api/ai/grounding"
	"github.com/sentry/sentry/api/ai/provider/ollama"
	"github.com/sentry/sentry/api/ai/router"
	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/dashboards"
	"github.com/sentry/sentry/api/httpserver"
	"github.com/sentry/sentry/api/internal/config"
	"github.com/sentry/sentry/api/localauth"
	"github.com/sentry/sentry/api/logretention"
	"github.com/sentry/sentry/api/queryapi"
	"github.com/sentry/sentry/api/querylang/executor"
	"github.com/sentry/sentry/api/searchclient"
)

// groundingRefreshInterval matches chwriter.Registry/search's
// ActiveTenantTracker's own one-minute refresh cadence -- no strong
// reason for a different number, and consistency means one interval to
// reason about across every "sample something periodically" mechanism
// in this codebase, not several slightly different ones.
const groundingRefreshInterval = time.Minute

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}
	for _, w := range cfg.DevCredentialWarnings() {
		logger.Warn(w)
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

	// -seed-admin: a one-shot action, not part of the normal server
	// startup path -- mirrors enterprise-api's -provision-tenant shape
	// (declare, flag.Parse(), short-circuit before the rest of main's
	// dependencies matter to it). See runSeedAdmin's doc comment.
	seedAdmin := flag.Bool("seed-admin", false, "create the default local-auth admin user with a random password if none exists, print it once, and exit")
	flag.Parse()

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

	if *seedAdmin {
		os.Exit(runSeedAdmin(ctx, logger, os.Stdout, localauth.NewStore(pgPool)))
	}

	// authorizer is nil (RequireRole* becomes a no-op) unless
	// ENTERPRISE_AUTH_URL or LOCAL_AUTH_ENABLED is configured -- matches
	// Phase 0-3 behavior for a single-tenant deployment with neither
	// enterprise/ nor local login turned on. EnterpriseAuthURL wins if
	// both were somehow set -- a deployment with real SSO configured has
	// no use for a second, local auth mechanism (see LocalAuthConfig's
	// doc comment).
	var authorizer authz.Authorizer
	var localAuthStore *localauth.Store
	switch {
	case cfg.EnterpriseAuthURL != "":
		authorizer = authz.NewHTTPAuthorizer(cfg.EnterpriseAuthURL)
	case cfg.LocalAuth.Enabled:
		localAuthStore = localauth.NewStore(pgPool)
		authorizer = localauth.NewAuthorizer(localAuthStore)
	}

	sqlRunner := executor.NewChRunner(conn)
	// audit logging is nil (a no-op) until Phase 4 task 5 wires in
	// enterprise/internal/audit -- see queryapi.AuditLogger's doc comment.
	queryHandler := queryapi.NewHandler(logger, sqlRunner, search, cfg.QueryTimeout, nil, authorizer)
	// permissions is nil -- api/cmd/api is single-tenant/core; the
	// enterprise-supplied dashboard_permissions store is only wired in
	// by enterprise/cmd/enterprise-api (see dashboards.PermissionStore's
	// doc comment). Ownership/Admin access still work via
	// canEditDashboard's nil-permissions fallback -- only the "granted"
	// half of the matrix's "(own/granted)" qualifier is unavailable here.
	dashboardsHandler := dashboards.NewHandler(logger, dashboards.NewStore(pgPool), authorizer, nil)
	// Same pgPool as dashboards above -- agents reads/writes the table
	// ingest's internal/agentregistry upserts on every CheckIn RPC (see
	// /docs/agent-management-design.md). Nothing here requires
	// AGENT_REGISTRY_POSTGRES_ADDR to be set on ingest; these routes work
	// unconditionally, they'll just show an empty inventory if ingest
	// hasn't been configured to record check-ins.
	// nil command logger: core has no enterprise/internal/audit
	// implementation to log lifecycle commands against, same posture as
	// queryHandler's/aiHandler's nil audit loggers above.
	agentsHandler := agents.NewHandler(logger, agents.NewStore(pgPool), authorizer, nil)

	// Same conn sqlRunner above already wraps -- logretention issues its
	// own purpose-built statements against the `logs` table directly
	// rather than going through sqlRunner's SELECT-only RunSQL (see
	// logretention.Store's doc comment).
	logRetentionHandler := logretention.NewHandler(logger, logretention.NewStore(conn), authorizer)

	// One shared mux, CORS applied once around the whole thing -- see
	// httpserver's doc comment for why this changed from each
	// handler wrapping itself individually.
	mux := http.NewServeMux()
	queryHandler.RegisterRoutes(mux)
	dashboardsHandler.RegisterRoutes(mux)
	agentsHandler.RegisterRoutes(mux)
	logRetentionHandler.RegisterRoutes(mux)

	// Only registered when local auth is actually enabled -- see
	// localauth.Handler.RegisterRoutes' doc comment for why a disabled
	// deployment gets a plain 404 on /auth/* rather than a dedicated
	// "feature off" response.
	if localAuthStore != nil {
		localauthHandler := localauth.NewHandler(logger, localAuthStore, authorizer, cfg.LocalAuth.SessionTTL, localauth.CookieConfig{
			Domain: cfg.LocalAuth.CookieDomain,
			Secure: cfg.LocalAuth.CookieSecure,
		})
		localauthHandler.RegisterRoutes(mux)
	}

	// AI routes (Phase 7) are only registered at all when OLLAMA_BASE_URL
	// is set -- an unconfigured deployment gets a plain 404 on /ai/*
	// rather than every request failing against an unreachable
	// localhost:11434, matching "no cloud dependency required for the
	// default deployment" by not forcing a *local* model dependency on a
	// deployment that doesn't want AI features either.
	if cfg.AI.OllamaBaseURL != "" {
		groundingSvc := grounding.New(sqlRunner)
		groundingSvc.StartRefreshing(ctx, groundingRefreshInterval, func(err error) {
			logger.Warn("grounding refresh failed", "error", err)
		})

		defaultProvider := ollama.New(cfg.AI.OllamaBaseURL, cfg.AI.OllamaModel)
		aiRouter := router.New(defaultProvider)
		if cfg.AI.OllamaFastModel != "" && cfg.AI.OllamaFastModel != cfg.AI.OllamaModel {
			aiRouter.SetOperation(router.OpComplete, ollama.New(cfg.AI.OllamaBaseURL, cfg.AI.OllamaFastModel))
		}

		// nil interaction logger: core has no enterprise/internal/audit
		// implementation to log translate/fix/optimize interactions
		// against, same posture as queryHandler's nil audit logger above.
		aiHandler := aiapi.NewHandler(logger, aiRouter, groundingSvc, authorizer, nil)
		aiHandler.RegisterRoutes(mux)
		logger.Info("ai routes enabled", "ollama_base_url", cfg.AI.OllamaBaseURL, "model", cfg.AI.OllamaModel)
	}

	// Once an authorizer is live, requests carry a session cookie/bearer
	// token that must survive a cross-origin browser fetch --
	// WithCredentialedCORS is WithCORS's sibling for exactly that (see
	// httpserver/cors.go). This also fixes a latent gap: previously,
	// enterprise mode applied plain WithCORS here despite needing
	// cookies too.
	corsHandler := httpserver.WithCORS(mux, cfg.CORSAllowedOrigin)
	if authorizer != nil {
		corsHandler = httpserver.WithCredentialedCORS(mux, cfg.CORSAllowedOrigin)
	}
	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: corsHandler,
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

// runSeedAdmin is the operator action that bootstraps local login on a
// fresh deployment: idempotent (a no-op if any local user already
// exists, safe to run on every deploy per the runbook), so there's no
// separate "has this already run" flag to track. The generated
// password is printed to stdout exactly once and never stored in
// plaintext anywhere -- losing it means resetting it
// (POST /auth/users/{id}/reset-password), not recovering it.
func runSeedAdmin(ctx context.Context, logger *slog.Logger, stdout io.Writer, store *localauth.Store) int {
	n, err := store.CountLocalUsers(ctx)
	if err != nil {
		logger.Error("counting local users", "error", err)
		return 1
	}
	if n > 0 {
		fmt.Fprintln(stdout, "admin already provisioned, skipping")
		return 0
	}

	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		logger.Error("generating random password", "error", err)
		return 1
	}
	password := base64.RawURLEncoding.EncodeToString(buf)

	hash, err := localauth.HashPassword(password)
	if err != nil {
		logger.Error("hashing password", "error", err)
		return 1
	}
	if _, err := store.CreateUser(ctx, "admin", hash, authz.RoleOwner); err != nil {
		logger.Error("creating admin user", "error", err)
		return 1
	}

	fmt.Fprintln(stdout, "created default admin user:")
	fmt.Fprintln(stdout, "  username: admin")
	fmt.Fprintf(stdout, "  password: %s\n", password)
	fmt.Fprintln(stdout, "this password will not be shown again -- save it now.")
	return 0
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
