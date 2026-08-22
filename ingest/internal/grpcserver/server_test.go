package grpcserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cairnobs/cairnobs/ingest/internal/config"
	agentv1 "github.com/cairnobs/cairnobs/proto/sentry/agent/v1"
	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

type fakeProducer struct {
	mu      sync.Mutex
	written [][]kafka.Message
	err     error
}

func (f *fakeProducer) WriteBatch(_ context.Context, msgs []kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	batch := make([]kafka.Message, len(msgs))
	copy(batch, msgs)
	f.written = append(f.written, batch)
	return nil
}

// fakeResolver is an in-memory stand-in for
// ingest/internal/tenantresolver.HTTPResolver, keyed by token.
type fakeResolver struct {
	tenantByToken map[string]string
}

func (f *fakeResolver) ResolveTenant(_ context.Context, token string) (string, error) {
	tenantID, ok := f.tenantByToken[token]
	if !ok {
		return "", errors.New("fakeResolver: unknown token")
	}
	return tenantID, nil
}

// fakeAgentRegistry is an in-memory stand-in for
// ingest/internal/agentregistry.Registry, keyed by "tenantID/host" so
// tests can assert cross-tenant isolation the same way the real
// UNIQUE (tenant_id, host) constraint provides it.
type fakeAgentRegistry struct {
	mu        sync.Mutex
	checkIns  []AgentCheckIn
	overrides map[string]AgentOverride
	commands  map[string]string
	err       error
}

func (f *fakeAgentRegistry) CheckIn(_ context.Context, tenantID string, info AgentCheckIn) (CheckInResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return CheckInResult{}, f.err
	}
	f.checkIns = append(f.checkIns, info)
	key := tenantID + "/" + info.Host
	result := CheckInResult{Command: f.commands[key]}
	if f.overrides != nil {
		result.Override = f.overrides[key]
	}
	return result, nil
}

func newTestServer(p batchProducer) *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), config.GRPCConfig{}, config.TLSConfig{}, p, nil, nil)
}

func newTestServerWithResolver(p batchProducer, resolver TenantResolver) *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), config.GRPCConfig{}, config.TLSConfig{}, p, resolver, nil)
}

func newTestServerWithAgents(agents AgentRegistry) *Server {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), config.GRPCConfig{}, config.TLSConfig{}, &fakeProducer{}, nil, agents)
}

// contextWithBearerToken builds an incoming gRPC context carrying an
// "authorization: Bearer <token>" metadata entry -- the shape a real
// grpc-go server hands PushBatch once TLS/framing is stripped away, so
// this exercises the same metadata.FromIncomingContext path production
// traffic does, not a shortcut around it.
func contextWithBearerToken(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
}

func TestPushBatchAssignsRecordID(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	req := &logsv1.PushBatchRequest{
		BatchId: "b1",
		Records: []*logsv1.LogRecord{
			{Host: "h1", Message: "one"},
			{Host: "h1", Message: "two"},
		},
	}

	resp, err := s.PushBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}
	if resp.GetAccepted() != 2 {
		t.Fatalf("Accepted = %d, want 2", resp.GetAccepted())
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 1 || len(fp.written[0]) != 2 {
		t.Fatalf("unexpected written batches: %+v", fp.written)
	}

	seen := make(map[string]bool)
	for _, m := range fp.written[0] {
		var rec logsv1.LogRecord
		if err := proto.Unmarshal(m.Value, &rec); err != nil {
			t.Fatalf("unmarshaling produced message: %v", err)
		}
		if rec.GetRecordId() == "" {
			t.Fatalf("record_id was not assigned for message %q", rec.GetMessage())
		}
		if seen[rec.GetRecordId()] {
			t.Fatalf("duplicate record_id %q across records in the same batch", rec.GetRecordId())
		}
		seen[rec.GetRecordId()] = true
	}
}

func TestPushBatchOverwritesAgentSuppliedRecordID(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	req := &logsv1.PushBatchRequest{
		Records: []*logsv1.LogRecord{
			{Host: "h1", Message: "one", RecordId: "agent-supplied-should-be-ignored"},
		},
	}

	if _, err := s.PushBatch(context.Background(), req); err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	var rec logsv1.LogRecord
	if err := proto.Unmarshal(fp.written[0][0].Value, &rec); err != nil {
		t.Fatalf("unmarshaling produced message: %v", err)
	}
	if rec.GetRecordId() == "agent-supplied-should-be-ignored" {
		t.Fatal("expected ingest to overwrite any agent-supplied record_id")
	}
	if rec.GetRecordId() == "" {
		t.Fatal("expected a server-assigned record_id")
	}
}

func TestPushBatchEmptyRecordsIsANoOp(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	resp, err := s.PushBatch(context.Background(), &logsv1.PushBatchRequest{})
	if err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}
	if resp.GetAccepted() != 0 {
		t.Fatalf("Accepted = %d, want 0", resp.GetAccepted())
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 0 {
		t.Fatalf("expected no batches written for an empty request, got %d", len(fp.written))
	}
}

// TestPushBatchNoResolverAttachesNoTenantHeader is the regression test
// for single-tenant deployments' behavior staying unchanged: with no
// TenantResolver configured, records are produced exactly as before --
// no tenant_id header at all -- even with a bearer token present (it's
// simply never inspected).
func TestPushBatchNoResolverAttachesNoTenantHeader(t *testing.T) {
	fp := &fakeProducer{}
	s := newTestServer(fp)

	req := &logsv1.PushBatchRequest{Records: []*logsv1.LogRecord{{Host: "h1", Message: "one"}}}
	if _, err := s.PushBatch(contextWithBearerToken("irrelevant"), req); err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, h := range fp.written[0][0].Headers {
		if h.Key == TenantIDHeaderKey {
			t.Fatalf("expected no %s header with no resolver configured, got %q", TenantIDHeaderKey, h.Value)
		}
	}
}

func TestPushBatchWithResolverAttachesTenantHeader(t *testing.T) {
	fp := &fakeProducer{}
	resolver := &fakeResolver{tenantByToken: map[string]string{"real-token": "acme"}}
	s := newTestServerWithResolver(fp, resolver)

	req := &logsv1.PushBatchRequest{Records: []*logsv1.LogRecord{
		{Host: "h1", Message: "one"},
		{Host: "h1", Message: "two"},
	}}
	if _, err := s.PushBatch(contextWithBearerToken("real-token"), req); err != nil {
		t.Fatalf("PushBatch() error = %v", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written[0]) != 2 {
		t.Fatalf("expected 2 messages written, got %d", len(fp.written[0]))
	}
	for _, msg := range fp.written[0] {
		found := false
		for _, h := range msg.Headers {
			if h.Key == TenantIDHeaderKey {
				found = true
				if string(h.Value) != "acme" {
					t.Fatalf("%s header = %q, want acme", TenantIDHeaderKey, h.Value)
				}
			}
		}
		if !found {
			t.Fatalf("expected every record to carry a %s header", TenantIDHeaderKey)
		}
	}
}

func TestPushBatchWithResolverRejectsMissingToken(t *testing.T) {
	fp := &fakeProducer{}
	resolver := &fakeResolver{tenantByToken: map[string]string{"real-token": "acme"}}
	s := newTestServerWithResolver(fp, resolver)

	req := &logsv1.PushBatchRequest{Records: []*logsv1.LogRecord{{Host: "h1", Message: "one"}}}
	_, err := s.PushBatch(context.Background(), req) // no bearer token in context at all
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("PushBatch() error = %v, want Unauthenticated", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 0 {
		t.Fatal("a batch with no bearer token must never reach the producer once a resolver is configured")
	}
}

// TestPushBatchWithResolverRejectsInvalidToken is the fail-closed
// regression test: a resolver configured but a token it doesn't
// recognize must refuse the whole batch, never fall back to "no tenant"
// (which would silently defeat the point of requiring a credential at
// all).
func TestPushBatchWithResolverRejectsInvalidToken(t *testing.T) {
	fp := &fakeProducer{}
	resolver := &fakeResolver{tenantByToken: map[string]string{"real-token": "acme"}}
	s := newTestServerWithResolver(fp, resolver)

	req := &logsv1.PushBatchRequest{Records: []*logsv1.LogRecord{{Host: "h1", Message: "one"}}}
	_, err := s.PushBatch(contextWithBearerToken("wrong-token"), req)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("PushBatch() error = %v, want Unauthenticated", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.written) != 0 {
		t.Fatal("a batch with an invalid token must never reach the producer once a resolver is configured")
	}
}

func TestCheckInNilRegistryIsANoOp(t *testing.T) {
	s := newTestServer(&fakeProducer{})

	resp, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{Host: "h1", CurrentConfig: &agentv1.ReportedConfig{}})
	if err != nil {
		t.Fatalf("CheckIn() error = %v", err)
	}
	if resp.GetHasOverride() {
		t.Fatal("expected has_override=false with no AgentRegistry configured")
	}
}

func TestCheckInRejectsEmptyHost(t *testing.T) {
	s := newTestServer(&fakeProducer{})

	_, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{CurrentConfig: &agentv1.ReportedConfig{}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CheckIn() error = %v, want InvalidArgument", err)
	}
}

func TestCheckInRecordsReportedConfig(t *testing.T) {
	reg := &fakeAgentRegistry{}
	s := newTestServerWithAgents(reg)

	_, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{
		Host:    "web-01",
		Service: "web",
		CurrentConfig: &agentv1.ReportedConfig{
			AgentVersion:         "0.1.0",
			SourceKind:           "journald",
			BatchMaxSize:         500,
			BatchFlushIntervalMs: 2000,
			HeartbeatEnabled:     true,
			HeartbeatIntervalMs:  60000,
		},
		AppliedOverrideVersion: "v3",
	})
	if err != nil {
		t.Fatalf("CheckIn() error = %v", err)
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.checkIns) != 1 {
		t.Fatalf("expected 1 recorded check-in, got %d", len(reg.checkIns))
	}
	got := reg.checkIns[0]
	if got.Host != "web-01" || got.AgentVersion != "0.1.0" || got.BatchMaxSize != 500 || got.AppliedOverrideVersion != "v3" {
		t.Fatalf("unexpected recorded check-in: %+v", got)
	}
}

// TestCheckInReturnsOverrideWhenSet is the regression test for the
// actual point of this RPC: an operator-set override for this specific
// host comes back in the response, correctly shaped.
func TestCheckInReturnsOverrideWhenSet(t *testing.T) {
	// Keyed by "" (empty tenantID) rather than "default" -- the
	// empty-to-"default" substitution is agentregistry.Registry's own
	// Postgres-specific behavior (matching the seeded default tenant
	// row), not something grpcserver itself does; this fake exercises
	// grpcserver.CheckIn in isolation, so it sees the tenantID exactly
	// as resolveTenant produced it (empty, since no resolver is
	// configured for this test).
	interval := uint64(30000)
	reg := &fakeAgentRegistry{overrides: map[string]AgentOverride{
		"/web-01": {HasOverride: true, HeartbeatIntervalMS: &interval, Version: "v2"},
	}}
	s := newTestServerWithAgents(reg)

	resp, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{Host: "web-01", CurrentConfig: &agentv1.ReportedConfig{}})
	if err != nil {
		t.Fatalf("CheckIn() error = %v", err)
	}
	if !resp.GetHasOverride() {
		t.Fatal("expected has_override=true")
	}
	if resp.GetOverride().GetHeartbeatIntervalMs() != 30000 || resp.GetOverride().GetVersion() != "v2" {
		t.Fatalf("unexpected override: %+v", resp.GetOverride())
	}
}

func TestCheckInWithResolverRejectsMissingToken(t *testing.T) {
	resolver := &fakeResolver{tenantByToken: map[string]string{"real-token": "acme"}}
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), config.GRPCConfig{}, config.TLSConfig{}, &fakeProducer{}, resolver, &fakeAgentRegistry{})

	_, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{Host: "web-01", CurrentConfig: &agentv1.ReportedConfig{}})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("CheckIn() error = %v, want Unauthenticated", err)
	}
}

func TestCheckInDeliversPendingRestartCommand(t *testing.T) {
	reg := &fakeAgentRegistry{commands: map[string]string{"/web-01": AgentCommandRestart}}
	s := newTestServerWithAgents(reg)

	resp, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{Host: "web-01", CurrentConfig: &agentv1.ReportedConfig{}})
	if err != nil {
		t.Fatalf("CheckIn() error = %v", err)
	}
	if resp.GetPendingCommand() != agentv1.AgentCommand_AGENT_COMMAND_RESTART {
		t.Fatalf("PendingCommand = %v, want AGENT_COMMAND_RESTART", resp.GetPendingCommand())
	}
}

func TestCheckInNoCommandReportsUnspecified(t *testing.T) {
	reg := &fakeAgentRegistry{}
	s := newTestServerWithAgents(reg)

	resp, err := s.CheckIn(context.Background(), &agentv1.CheckInRequest{Host: "web-01", CurrentConfig: &agentv1.ReportedConfig{}})
	if err != nil {
		t.Fatalf("CheckIn() error = %v", err)
	}
	if resp.GetPendingCommand() != agentv1.AgentCommand_AGENT_COMMAND_UNSPECIFIED {
		t.Fatalf("PendingCommand = %v, want AGENT_COMMAND_UNSPECIFIED", resp.GetPendingCommand())
	}
}
