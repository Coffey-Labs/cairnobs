// Command api is Sentry's query API: a single POST /query endpoint
// accepting either the pipe syntax or raw SQL, compiled and routed
// across ClickHouse and search by internal/querylang. See
// internal/queryapi and /docs/query-language-design.md for why this is
// plain REST rather than the pinned gRPC+gateway pattern.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/sentry/sentry/api/internal/config"
	"github.com/sentry/sentry/api/internal/querylang/executor"
	"github.com/sentry/sentry/api/internal/queryapi"
	"github.com/sentry/sentry/api/internal/searchclient"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
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

	sqlRunner := executor.NewChRunner(conn)
	handler := queryapi.NewHandler(logger, sqlRunner, search, cfg.QueryTimeout, cfg.CORSAllowedOrigin)

	srv := &http.Server{
		Addr:    cfg.HTTPListenAddr,
		Handler: handler.Routes(),
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
