// Package grpcserver implements the agent-facing side of ingest: an mTLS
// gRPC server accepting LogIngest.PushBatch calls. It assigns each record
// a stable record_id (see the proto field comment for why this has to
// happen exactly once, here, rather than in either downstream consumer)
// and otherwise forwards records unchanged onto Redpanda — normalization
// into the ClickHouse row shape happens later, on the consumer side.
//
// If a TenantResolver is configured, PushBatch also resolves which
// tenant the call's bearer credential belongs to and attaches it as a
// "tenant_id" Kafka message header on every record produced -- the first
// step of Phase 4's ingest tenant-awareness (see
// /docs/phase-4-runbook.md and CLAUDE.md's "ingest itself has no tenant
// concept" gap). Deliberately scoped no further than that for now:
// nothing downstream (this package's own consumer, or `search`'s
// separate Redpanda consumer) reads that header yet to route a record's
// write into a per-tenant ClickHouse database/Tantivy index -- every
// record still lands in the one shared destination either way, tenant_id
// header or not. That's real, disclosed, deferred follow-up work, not
// silently incomplete: attaching a verifiable tenant identity as early
// as possible (right where the credential is actually presented) is a
// self-contained, independently valuable step on its own, and it's what
// any later per-tenant write-routing work will consume.
package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/sentry/sentry/ingest/internal/config"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

// TenantIDHeaderKey is the Kafka message header a resolved tenant ID is
// attached under -- exported so internal/consumer (or a future per-
// tenant write-routing consumer) can read it back by the same name
// without duplicating the literal.
const TenantIDHeaderKey = "tenant_id"

type Server struct {
	logsv1.UnimplementedLogIngestServer

	logger   *slog.Logger
	grpcCfg  config.GRPCConfig
	tlsCfg   config.TLSConfig
	producer batchProducer
	resolver TenantResolver
}

// batchProducer is the subset of *producer.Producer this package depends
// on, so tests can substitute a fake without touching Redpanda.
type batchProducer interface {
	WriteBatch(ctx context.Context, msgs []kafka.Message) error
}

// TenantResolver validates an ingest credential (a bearer token
// presented via gRPC metadata, `authorization: Bearer <token>`) and
// resolves which tenant it belongs to. nil is a deliberate no-op: every
// record's Kafka message gets no tenant_id header at all, matching every
// ingest deployment's behavior before per-tenant ingest credentials
// existed. The real implementation
// (ingest/internal/tenantresolver.HTTPResolver) is a plain HTTP client
// calling enterprise-auth's /internal/authorize-ingest -- never an
// enterprise/ import, since this package is AGPL core (same "network
// boundary, not import boundary" shape api/authz.Authorizer uses).
type TenantResolver interface {
	ResolveTenant(ctx context.Context, token string) (tenantID string, err error)
}

func New(logger *slog.Logger, grpcCfg config.GRPCConfig, tlsCfg config.TLSConfig, p batchProducer, resolver TenantResolver) *Server {
	return &Server{logger: logger, grpcCfg: grpcCfg, tlsCfg: tlsCfg, producer: p, resolver: resolver}
}

// Run blocks serving gRPC until ctx is canceled, then gracefully stops.
func (s *Server) Run(ctx context.Context) error {
	tlsConf, err := loadServerTLSConfig(s.tlsCfg)
	if err != nil {
		return fmt.Errorf("loading TLS config: %w", err)
	}

	lis, err := net.Listen("tcp", s.grpcCfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.grpcCfg.ListenAddr, err)
	}

	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConf)))
	logsv1.RegisterLogIngestServer(grpcSrv, s)

	s.logger.Info("gRPC server listening", "addr", s.grpcCfg.ListenAddr)

	errCh := make(chan error, 1)
	go func() { errCh <- grpcSrv.Serve(lis) }()

	select {
	case <-ctx.Done():
		grpcSrv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) PushBatch(ctx context.Context, req *logsv1.PushBatchRequest) (*logsv1.PushBatchResponse, error) {
	if len(req.GetRecords()) == 0 {
		return &logsv1.PushBatchResponse{Accepted: 0}, nil
	}

	// tenantID stays empty (no header attached below) unless a resolver
	// is actually configured -- single-tenant deployments never present
	// a bearer credential and never need to. Once a resolver IS
	// configured, a missing/invalid credential fails the whole batch
	// closed rather than falling back to "no tenant" -- exactly the
	// same fail-closed shape enterprise/internal/chrunner.Registry.RunSQL
	// uses on the read side, applied here at the point data enters the
	// system.
	var tenantID string
	if s.resolver != nil {
		token, ok := bearerTokenFromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing bearer credential")
		}
		resolved, err := s.resolver.ResolveTenant(ctx, token)
		if err != nil {
			s.logger.Error("resolving ingest tenant", "batch_id", req.GetBatchId(), "error", err)
			return nil, status.Error(codes.Unauthenticated, "invalid ingest credential")
		}
		tenantID = resolved
	}

	msgs := make([]kafka.Message, 0, len(req.GetRecords()))
	for _, rec := range req.GetRecords() {
		// Assigned here, once, before this record is produced to
		// Redpanda: the ClickHouse-writer consumer and the Tantivy-
		// indexer consumer (Phase 1) both read the same Redpanda
		// messages and need to agree on the same ID for the same
		// record. Overwrites anything the agent sent (it always sends
		// empty, per the proto comment, but this is authoritative
		// regardless).
		rec.RecordId = uuid.NewString()

		val, err := proto.Marshal(rec)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "marshaling record: %v", err)
		}
		msg := kafka.Message{
			Key:   []byte(rec.GetHost()),
			Value: val,
		}
		if tenantID != "" {
			msg.Headers = []kafka.Header{{Key: TenantIDHeaderKey, Value: []byte(tenantID)}}
		}
		msgs = append(msgs, msg)
	}

	if err := s.producer.WriteBatch(ctx, msgs); err != nil {
		s.logger.Error("failed to write batch to redpanda", "batch_id", req.GetBatchId(), "error", err)
		return nil, status.Errorf(codes.Unavailable, "writing to transport: %v", err)
	}

	s.logger.Debug("batch produced to redpanda", "batch_id", req.GetBatchId(), "records", len(req.GetRecords()), "tenant_id", tenantID)
	return &logsv1.PushBatchResponse{Accepted: uint32(len(req.GetRecords()))}, nil
}

// bearerTokenFromContext reads the same "authorization: Bearer <token>"
// gRPC metadata shape HTTP's Authorization header uses -- an agent sets
// this once per PushBatch call (see the agent's grpc.rs), not per
// record.
func bearerTokenFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(values[0], prefix) {
		return "", false
	}
	return strings.TrimPrefix(values[0], prefix), true
}
