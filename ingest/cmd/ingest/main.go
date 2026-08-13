// Command ingest is the Sentry ingest service. It has two halves that can
// run in one process or be split across deployments via --mode:
//
//   - server:   mTLS gRPC front end that agents push batches to; forwards
//     them onto Redpanda unchanged.
//   - consumer: reads back off Redpanda, normalizes, batch-writes to
//     ClickHouse.
//   - all (default): both, in one process — the Phase 0 / docker-compose
//     shape. Splitting into separate deployments later is a k8s manifest
//     change, not a code change.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/sentry/sentry/ingest/internal/clickhousewriter"
	"github.com/sentry/sentry/ingest/internal/config"
	"github.com/sentry/sentry/ingest/internal/consumer"
	"github.com/sentry/sentry/ingest/internal/grpcserver"
	"github.com/sentry/sentry/ingest/internal/producer"
)

func main() {
	mode := flag.String("mode", "all", "which half of ingest to run: server | consumer | all")
	flag.Parse()

	if *mode != "server" && *mode != "consumer" && *mode != "all" {
		fmt.Fprintf(os.Stderr, "unknown --mode %q, must be server|consumer|all\n", *mode)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g, ctx := errgroup.WithContext(ctx)

	if *mode == "server" || *mode == "all" {
		p := producer.New(cfg.Redpanda)
		defer p.Close()
		srv := grpcserver.New(logger, cfg.GRPC, cfg.TLS, p)
		g.Go(func() error { return srv.Run(ctx) })
	}

	if *mode == "consumer" || *mode == "all" {
		chw, err := clickhousewriter.New(ctx, cfg.ClickHouse)
		if err != nil {
			logger.Error("connecting to clickhouse", "error", err)
			os.Exit(1)
		}
		defer chw.Close()
		c := consumer.New(logger, cfg.Redpanda, cfg.Batch, chw)
		g.Go(func() error { return c.Run(ctx) })
	}

	logger.Info("ingest started", "mode", *mode)

	if err := g.Wait(); err != nil {
		logger.Error("ingest exited with error", "error", err)
		os.Exit(1)
	}
}
