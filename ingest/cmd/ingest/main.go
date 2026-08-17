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

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/sentry/sentry/ingest/clickhousewriter"
	"github.com/sentry/sentry/ingest/consumer"
	"github.com/sentry/sentry/ingest/internal/agentregistry"
	"github.com/sentry/sentry/ingest/internal/config"
	"github.com/sentry/sentry/ingest/internal/grpcserver"
	"github.com/sentry/sentry/ingest/internal/producer"
	"github.com/sentry/sentry/ingest/internal/tenantresolver"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// singleTenantWriter adapts *clickhousewriter.Writer -- which only
// knows how to write to the one ClickHouse database it was constructed
// with -- to consumer.chWriter's tenant-tagged signature, by simply
// ignoring the tag. This is this binary's single-tenant behavior,
// unchanged from before per-tenant ingest credentials existed: every
// record lands in the same shared database regardless of which tenant
// (if any) it was resolved to. enterprise/cmd/enterprise-ingest is
// where a tag-respecting writer (enterprise/internal/chwriter.Registry)
// actually routes per tenant instead.
type singleTenantWriter struct {
	w *clickhousewriter.Writer
}

func (s singleTenantWriter) WriteBatch(ctx context.Context, records []consumer.Record) error {
	plain := make([]*logsv1.LogRecord, len(records))
	for i, r := range records {
		plain[i] = r.Record
	}
	return s.w.WriteBatch(ctx, plain)
}

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
		// resolver stays nil (every batch's tenant_id header is simply
		// never set) unless ENTERPRISE_AUTH_URL is configured -- matches
		// every other "off unless configured" optional dependency in
		// this codebase.
		var resolver grpcserver.TenantResolver
		if cfg.EnterpriseAuthURL != "" {
			resolver = tenantresolver.New(cfg.EnterpriseAuthURL)
			logger.Info("ingest tenant resolution configured", "enterprise_auth_url", cfg.EnterpriseAuthURL)
		} else {
			logger.Info("ENTERPRISE_AUTH_URL not set -- ingest records carry no tenant_id, single-tenant behavior")
		}

		// agents stays nil (CheckIn always reports "no override," nothing
		// recorded) unless AGENT_REGISTRY_POSTGRES_ADDR is configured --
		// same "off unless configured" shape as resolver above. Uses its
		// own pgxpool rather than sharing one across mode=server/consumer
		// -- consumer's half of this binary has no Postgres dependency at
		// all today and shouldn't gain one just because server's did.
		var agents grpcserver.AgentRegistry
		if cfg.AgentRegistry.Postgres.Addr != "" {
			dsn := fmt.Sprintf("postgres://%s:%s@%s/%s",
				cfg.AgentRegistry.Postgres.Username, cfg.AgentRegistry.Postgres.Password,
				cfg.AgentRegistry.Postgres.Addr, cfg.AgentRegistry.Postgres.Database)
			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				logger.Error("opening agent registry postgres pool", "error", err)
				os.Exit(1)
			}
			defer pool.Close()
			agents = agentregistry.New(pool)
			logger.Info("agent registry configured", "postgres_addr", cfg.AgentRegistry.Postgres.Addr)
		} else {
			logger.Info("AGENT_REGISTRY_POSTGRES_ADDR not set -- agent check-ins are accepted but not recorded, no remote config")
		}

		srv := grpcserver.New(logger, cfg.GRPC, cfg.TLS, p, resolver, agents)
		g.Go(func() error { return srv.Run(ctx) })
	}

	if *mode == "consumer" || *mode == "all" {
		chw, err := clickhousewriter.New(ctx, clickhousewriter.Config{
			Addr: cfg.ClickHouse.Addr, Database: cfg.ClickHouse.Database,
			Username: cfg.ClickHouse.Username, Password: cfg.ClickHouse.Password,
		})
		if err != nil {
			logger.Error("connecting to clickhouse", "error", err)
			os.Exit(1)
		}
		defer chw.Close()
		c := consumer.New(logger, consumer.Config{
			Brokers: cfg.Redpanda.Brokers, Topic: cfg.Redpanda.Topic, ConsumerGroup: cfg.Redpanda.ConsumerGroup,
			BatchMaxSize: cfg.Batch.MaxSize, FlushIntervalMS: cfg.Batch.FlushIntervalMS,
		}, singleTenantWriter{w: chw})
		g.Go(func() error { return c.Run(ctx) })
	}

	logger.Info("ingest started", "mode", *mode)

	if err := g.Wait(); err != nil {
		logger.Error("ingest exited with error", "error", err)
		os.Exit(1)
	}
}
