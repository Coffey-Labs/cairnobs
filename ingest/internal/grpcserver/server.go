// Package grpcserver implements the agent-facing side of ingest: an mTLS
// gRPC server accepting LogIngest.PushBatch calls, which it forwards
// unchanged (proto-encoded) onto Redpanda. Normalization into the
// ClickHouse row shape happens later, on the consumer side.
package grpcserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/sentry/sentry/ingest/internal/config"
	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

type Server struct {
	logsv1.UnimplementedLogIngestServer

	logger   *slog.Logger
	grpcCfg  config.GRPCConfig
	tlsCfg   config.TLSConfig
	producer batchProducer
}

// batchProducer is the subset of *producer.Producer this package depends
// on, so tests can substitute a fake without touching Redpanda.
type batchProducer interface {
	WriteBatch(ctx context.Context, msgs []kafka.Message) error
}

func New(logger *slog.Logger, grpcCfg config.GRPCConfig, tlsCfg config.TLSConfig, p batchProducer) *Server {
	return &Server{logger: logger, grpcCfg: grpcCfg, tlsCfg: tlsCfg, producer: p}
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

	msgs := make([]kafka.Message, 0, len(req.GetRecords()))
	for _, rec := range req.GetRecords() {
		val, err := proto.Marshal(rec)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "marshaling record: %v", err)
		}
		msgs = append(msgs, kafka.Message{
			Key:   []byte(rec.GetHost()),
			Value: val,
		})
	}

	if err := s.producer.WriteBatch(ctx, msgs); err != nil {
		s.logger.Error("failed to write batch to redpanda", "batch_id", req.GetBatchId(), "error", err)
		return nil, status.Errorf(codes.Unavailable, "writing to transport: %v", err)
	}

	s.logger.Debug("batch produced to redpanda", "batch_id", req.GetBatchId(), "records", len(req.GetRecords()))
	return &logsv1.PushBatchResponse{Accepted: uint32(len(req.GetRecords()))}, nil
}
