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
// /docs/phase-4-runbook.md and PROJECT-SPEC.md's "ingest itself has no tenant
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

	"github.com/cairnobs/cairnobs/ingest/internal/config"
	agentv1 "github.com/cairnobs/cairnobs/proto/sentry/agent/v1"
	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// TenantIDHeaderKey is the Kafka message header a resolved tenant ID is
// attached under. ingest/consumer.TenantIDHeaderKey names the identical
// literal on the read side -- duplicated rather than imported (this
// package is the agent-facing producer side; consumer is a different
// concern, and importing across them for one string constant isn't
// worth the coupling), so a change here must be mirrored there.
const TenantIDHeaderKey = "tenant_id"

type Server struct {
	logsv1.UnimplementedLogIngestServer
	agentv1.UnimplementedAgentControlServer

	logger   *slog.Logger
	grpcCfg  config.GRPCConfig
	tlsCfg   config.TLSConfig
	producer batchProducer
	resolver TenantResolver
	agents   AgentRegistry
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

// AgentRegistry records an agent's CheckIn (for the web UI's inventory
// view) and returns any remote config override an operator has set for
// it. nil is a deliberate no-op, same "off unless configured" shape as
// TenantResolver: CheckIn always succeeds and reports "no override" --
// a deployment that hasn't configured AGENT_REGISTRY_POSTGRES_ADDR
// simply doesn't get agent inventory/management, exactly like one
// without ENTERPRISE_AUTH_URL doesn't get tenant-tagged records.
type AgentRegistry interface {
	CheckIn(ctx context.Context, tenantID string, info AgentCheckIn) (CheckInResult, error)
}

// CheckInResult bundles the two independent things a CheckIn can hand
// back to an agent -- a persistent config override to converge to, and
// a one-shot command to act on immediately. Kept as two separate
// concepts (not folded into one "override" shape) since their delivery
// semantics differ: Override is re-offered every CheckIn until the
// agent's applied_override_version matches; Command is cleared the
// instant it's handed out (see agent_control.proto's CheckInResponse
// comment).
type CheckInResult struct {
	Override AgentOverride
	// Command is AgentCommandRestart or "" (nothing pending). A plain
	// string, not the generated proto enum type, so AgentRegistry
	// implementations don't need to import the gRPC-facing package --
	// same reasoning as AgentCheckIn/AgentOverride below.
	Command string
}

// AgentCommandRestart is the one supported value for
// CheckInResult.Command / the agents table's pending_command column --
// see agent_control.proto's AgentCommand enum comment for why STOP/
// UNINSTALL aren't here yet.
const AgentCommandRestart = "restart"

// AgentCheckIn is what an agent reports about itself on each CheckIn --
// a plain-Go mirror of agentv1.ReportedConfig plus the identity/
// tenant fields, kept separate from the proto type so AgentRegistry
// implementations (ingest/internal/agentregistry) don't need to import
// this package's gRPC-facing types just to satisfy the interface.
type AgentCheckIn struct {
	Host                   string
	Service                string
	AgentVersion           string
	SourceKind             string
	SourceDetail           string
	BatchMaxSize           uint64
	BatchFlushIntervalMS   uint64
	HeartbeatEnabled       bool
	HeartbeatIntervalMS    uint64
	AppliedOverrideVersion string
}

// AgentOverride is the remotely-editable subset of an agent's config, as
// currently stored for it -- a plain-Go mirror of agentv1.DesiredOverride.
// Every pointer field is nil when that field has no override set.
// HasOverride false means no override has ever been set at all (Version
// is meaningless in that case).
type AgentOverride struct {
	HasOverride          bool
	BatchMaxSize         *uint64
	BatchFlushIntervalMS *uint64
	HeartbeatEnabled     *bool
	HeartbeatIntervalMS  *uint64
	JournaldUnit         *string
	// Extra file paths this agent should tail in addition to its local
	// [source] -- see agent_control.proto's DesiredOverride.
	// extra_file_paths comment for why this has no "unset" state the
	// way the pointer fields above do.
	ExtraFilePaths []string
	Version        string
}

func New(logger *slog.Logger, grpcCfg config.GRPCConfig, tlsCfg config.TLSConfig, p batchProducer, resolver TenantResolver, agents AgentRegistry) *Server {
	return &Server{logger: logger, grpcCfg: grpcCfg, tlsCfg: tlsCfg, producer: p, resolver: resolver, agents: agents}
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
	agentv1.RegisterAgentControlServer(grpcSrv, s)

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

	tenantID, err := s.resolveTenant(ctx)
	if err != nil {
		s.logger.Error("resolving ingest tenant", "batch_id", req.GetBatchId(), "error", err)
		return nil, err
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

// resolveTenant is PushBatch's and CheckIn's shared tenant-resolution
// step, extracted so CheckIn gets the identical fail-closed behavior
// without duplicating it: empty tenantID (no resolver configured, the
// single-tenant default) is not an error, but a configured resolver
// that gets no/an invalid credential is -- exactly the same posture
// enterprise/internal/chrunner.Registry.RunSQL uses on the read side,
// applied here at the point data (or a check-in) enters the system.
func (s *Server) resolveTenant(ctx context.Context) (string, error) {
	if s.resolver == nil {
		return "", nil
	}
	token, ok := bearerTokenFromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing bearer credential")
	}
	resolved, err := s.resolver.ResolveTenant(ctx, token)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "invalid ingest credential")
	}
	return resolved, nil
}

// CheckIn is AgentControl's one RPC (see agent_control.proto) -- agent-
// initiated, on its own heartbeat ticker. A nil AgentRegistry (no
// AGENT_REGISTRY_POSTGRES_ADDR configured) makes this a pure no-op that
// always reports "no override," so agents calling in against a
// deployment that hasn't opted into this feature see no behavior
// change at all.
func (s *Server) CheckIn(ctx context.Context, req *agentv1.CheckInRequest) (*agentv1.CheckInResponse, error) {
	if req.GetHost() == "" {
		return nil, status.Error(codes.InvalidArgument, "host must not be empty")
	}

	tenantID, err := s.resolveTenant(ctx)
	if err != nil {
		s.logger.Error("resolving ingest tenant for check-in", "host", req.GetHost(), "error", err)
		return nil, err
	}

	if s.agents == nil {
		return &agentv1.CheckInResponse{HasOverride: false}, nil
	}

	cfg := req.GetCurrentConfig()
	result, err := s.agents.CheckIn(ctx, tenantID, AgentCheckIn{
		Host:                   req.GetHost(),
		Service:                req.GetService(),
		AgentVersion:           cfg.GetAgentVersion(),
		SourceKind:             cfg.GetSourceKind(),
		SourceDetail:           cfg.GetSourceDetail(),
		BatchMaxSize:           cfg.GetBatchMaxSize(),
		BatchFlushIntervalMS:   cfg.GetBatchFlushIntervalMs(),
		HeartbeatEnabled:       cfg.GetHeartbeatEnabled(),
		HeartbeatIntervalMS:    cfg.GetHeartbeatIntervalMs(),
		AppliedOverrideVersion: req.GetAppliedOverrideVersion(),
	})
	if err != nil {
		s.logger.Error("recording agent check-in", "host", req.GetHost(), "error", err)
		return nil, status.Errorf(codes.Internal, "recording check-in: %v", err)
	}

	resp := &agentv1.CheckInResponse{HasOverride: result.Override.HasOverride}
	if result.Override.HasOverride {
		resp.Override = &agentv1.DesiredOverride{
			BatchMaxSize:         result.Override.BatchMaxSize,
			BatchFlushIntervalMs: result.Override.BatchFlushIntervalMS,
			HeartbeatEnabled:     result.Override.HeartbeatEnabled,
			HeartbeatIntervalMs:  result.Override.HeartbeatIntervalMS,
			JournaldUnit:         result.Override.JournaldUnit,
			ExtraFilePaths:       result.Override.ExtraFilePaths,
			Version:              result.Override.Version,
		}
	}
	switch result.Command {
	case AgentCommandRestart:
		resp.PendingCommand = agentv1.AgentCommand_AGENT_COMMAND_RESTART
		s.logger.Info("delivering restart command to agent", "host", req.GetHost())
	case "":
		// nothing pending
	default:
		s.logger.Error("agent registry returned an unknown command, ignoring", "host", req.GetHost(), "command", result.Command)
	}
	return resp, nil
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
