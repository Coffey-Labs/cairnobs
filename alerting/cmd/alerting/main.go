// Command alerting is Sentry's alert rule evaluator and delivery
// service: rule/target CRUD, the ticker-driven ok/pending/firing
// evaluator, and the webhook/Slack/PagerDuty delivery worker. See
// /docs/phase-3-alerting-design.md. Never talks to ClickHouse/Tantivy
// directly -- rule queries run through /api's POST /query
// (internal/queryclient), same precedent sentryctl query and the web
// UI's dashboard panels already set.
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

	"github.com/sentry/sentry/alerting/internal/config"
	"github.com/sentry/sentry/alerting/internal/delivery"
	"github.com/sentry/sentry/alerting/internal/evaluator"
	"github.com/sentry/sentry/alerting/internal/httpapi"
	"github.com/sentry/sentry/alerting/internal/httpserver"
	"github.com/sentry/sentry/alerting/internal/notifystore"
	"github.com/sentry/sentry/alerting/internal/queryclient"
	"github.com/sentry/sentry/alerting/internal/rulestore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	// -healthcheck: self-check mode for Docker's HEALTHCHECK, mirrors
	// api/cmd/api/main.go's runHealthcheck -- this image is distroless too
	// (no shell, no wget), so the compose healthcheck execs this binary
	// against itself.
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

	rules := rulestore.NewStore(pgPool)
	targets := notifystore.NewStore(pgPool)
	qc := queryclient.New(cfg.APIQueryURL)

	handler := httpapi.NewHandler(logger, rules, targets, rules)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: httpserver.WithCORS(mux, cfg.CORSAllowedOrigin),
	}

	eval := evaluator.New(rules, targets, qc, cfg.Evaluator.QueryTimeout, cfg.Evaluator.ClaimBatchSize, cfg.Evaluator.WorkerPoolSize, logger)
	deliveryWorker := delivery.NewWorker(pgPool, targets, logger)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("alerting listening", "addr", cfg.HTTPListenAddr)
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("graceful shutdown: %w", err)
			}
			return nil
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("http server exited: %w", err)
			}
			return nil
		}
	})

	g.Go(func() error {
		logger.Info("evaluator started", "tick_interval", cfg.Evaluator.TickInterval, "worker_pool_size", cfg.Evaluator.WorkerPoolSize)
		if err := eval.Run(ctx, cfg.Evaluator.TickInterval); err != nil && ctx.Err() == nil {
			return fmt.Errorf("evaluator exited: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("delivery worker started")
		if err := deliveryWorker.Run(ctx, cfg.Evaluator.TickInterval); err != nil && ctx.Err() == nil {
			return fmt.Errorf("delivery worker exited: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		logger.Error("alerting exited with error", "error", err)
		os.Exit(1)
	}
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
