// Command enterprise-ingest is the multi-tenant-aware alternative to
// running `ingest -mode=consumer` -- reads the same shared
// sentry.logs.raw Redpanda topic ingest/cmd/ingest's agent-facing
// server half (PushBatch) produces onto (see that binary's doc
// comment), but writes each record into its own tenant's dedicated
// ClickHouse database (enterprise/internal/chwriter) instead of the one
// shared table `ingest -mode=consumer` always writes to.
//
// Why a second binary, not a flag on ingest/cmd/ingest: ingest is AGPL
// core and must never import enterprise/ (hack/check-tenant-boundary.sh
// enforces this) -- there is no way for ingest's own binary to
// construct an enterprise-supplied chwriter.Registry (which needs
// rbacstore's per-tenant ClickHouse credentials) without that import.
// enterprise/ importing ingest/ is the allowed direction, so this
// binary lives here instead, reusing ingest/consumer.Consumer's own
// flush loop unchanged with a tenant-aware writer swapped in -- the
// exact same "second binary" shape as enterprise/cmd/enterprise-api
// next to api/cmd/api.
//
// A real multi-tenant deployment runs this binary INSTEAD OF (not
// alongside) `ingest -mode=consumer` -- `ingest -mode=server` (the
// agent-facing half, which tags records with a tenant_id via
// TenantResolver) keeps running unchanged and unconditionally either
// way; only which process consumes sentry.logs.raw and where it writes
// changes.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/sentry/sentry/enterprise/internal/chwriter"
	"github.com/sentry/sentry/enterprise/internal/ingestconfig"
	"github.com/sentry/sentry/enterprise/internal/rbacstore"
	"github.com/sentry/sentry/ingest/consumer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := ingestconfig.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck(cfg.HTTPListenAddr))
	}

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

	// Same source of truth chrunner.Registry (the read side) already
	// uses -- active+credentialed tenants only, see
	// rbacstore.ListProvisionedDataSources's doc comment. A tenant
	// that's mid-provisioning simply has no writer in the registry
	// below, so chwriter.Registry.WriteBatch refuses it the same way
	// chrunner.Registry.RunSQL already refuses an unprovisioned tenant
	// on the read side.
	sources, err := rbac.ListProvisionedDataSources(ctx)
	if err != nil {
		logger.Error("listing provisioned data sources", "error", err)
		os.Exit(1)
	}
	chwSources := make([]chwriter.DataSource, 0, len(sources))
	for _, s := range sources {
		if s.ClickHouseUsername == nil || s.ClickHousePassword == nil {
			continue // ListProvisionedDataSources already filters these out; defensive only.
		}
		chwSources = append(chwSources, chwriter.DataSource{
			TenantID: s.TenantID, Database: s.ClickHouseDatabaseName,
			Username: *s.ClickHouseUsername, Password: *s.ClickHousePassword,
		})
	}
	logger.Info("loaded tenant data sources", "count", len(chwSources))

	registry, err := chwriter.New(ctx, cfg.ClickHouseAddr, chwSources)
	if err != nil {
		logger.Error("building tenant write registry", "error", err)
		os.Exit(1)
	}
	defer registry.Close()

	c := consumer.New(logger, consumer.Config{
		Brokers: cfg.Redpanda.Brokers, Topic: cfg.Redpanda.Topic, ConsumerGroup: cfg.Redpanda.ConsumerGroup,
		BatchMaxSize: cfg.Batch.MaxSize, FlushIntervalMS: cfg.Batch.FlushIntervalMS,
	}, registry)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: cfg.HTTPListenAddr, Handler: mux}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.Run(ctx) })
	g.Go(func() error {
		logger.Info("enterprise-ingest healthz listening", "addr", cfg.HTTPListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})

	logger.Info("enterprise-ingest started")
	if err := g.Wait(); err != nil {
		logger.Error("enterprise-ingest exited with error", "error", err)
		os.Exit(1)
	}
}

// runHealthcheck mirrors every other binary in this repo's
// -healthcheck self-check mode -- execs the binary against itself
// rather than using an external tool (see e.g. api/cmd/api/main.go's
// runHealthcheck doc comment).
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
