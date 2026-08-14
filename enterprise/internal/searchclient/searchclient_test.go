// Tests run a real gRPC server in-process (net.Listen on an ephemeral
// port, a real grpc.Server) implementing SearchServiceServer, so these
// exercise the actual wire protocol Client.Search sends, not a mocked
// interface -- proving TenantId is set on the real SearchRequest that
// would reach `search`, and that Client fails closed exactly the way
// enterprise/internal/chrunner.Registry.RunSQL does for ClickHouse.
package searchclient

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/sentry/sentry/api/authz"
	searchv1 "github.com/sentry/sentry/proto/sentry/search/v1"
)

type fakeSearchServer struct {
	searchv1.UnimplementedSearchServiceServer
	lastRequest *searchv1.SearchRequest
	recordIDs   []string
}

func (f *fakeSearchServer) Search(_ context.Context, req *searchv1.SearchRequest) (*searchv1.SearchResponse, error) {
	f.lastRequest = req
	return &searchv1.SearchResponse{RecordIds: f.recordIDs}, nil
}

func newTestServer(t *testing.T) (*Client, *fakeSearchServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	fake := &fakeSearchServer{recordIDs: []string{"id-1", "id-2"}}
	srv := grpc.NewServer()
	searchv1.RegisterSearchServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, fake
}

func TestSearchForwardsTenantIDFromContext(t *testing.T) {
	client, fake := newTestServer(t)
	ctx := authz.WithIdentity(context.Background(), authz.Identity{TenantID: "acme", Role: authz.RoleViewer})

	ids, err := client.Search(ctx, "error", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d record ids, want 2", len(ids))
	}
	if fake.lastRequest.TenantId != "acme" {
		t.Fatalf("TenantId sent to search = %q, want acme", fake.lastRequest.TenantId)
	}
	if fake.lastRequest.Query != "error" || fake.lastRequest.Limit != 10 {
		t.Fatalf("unexpected request: %+v", fake.lastRequest)
	}
}

func TestSearchRefusesWithNoIdentity(t *testing.T) {
	client, fake := newTestServer(t)

	if _, err := client.Search(context.Background(), "error", 10); err == nil {
		t.Fatal("expected Search to refuse a request with no authenticated identity in context")
	}
	if fake.lastRequest != nil {
		t.Fatal("expected the gRPC call to never reach the server when there's no identity")
	}
}

func TestSearchRefusesIdentityWithNoTenant(t *testing.T) {
	client, fake := newTestServer(t)
	// RoleService identities carry no TenantID -- see api/authz.Identity's
	// doc comment. /alerting never calls search directly today, but the
	// fail-closed behavior must hold regardless of how this arises.
	ctx := authz.WithIdentity(context.Background(), authz.Identity{Role: authz.RoleService})

	if _, err := client.Search(ctx, "error", 10); err == nil {
		t.Fatal("expected Search to refuse an identity with no tenant")
	}
	if fake.lastRequest != nil {
		t.Fatal("expected the gRPC call to never reach the server when the identity has no tenant")
	}
}

func TestSearchDifferentTenantsSendDifferentTenantIDs(t *testing.T) {
	client, fake := newTestServer(t)

	ctxA := authz.WithIdentity(context.Background(), authz.Identity{TenantID: "acme", Role: authz.RoleViewer})
	if _, err := client.Search(ctxA, "q", 5); err != nil {
		t.Fatalf("Search (acme): %v", err)
	}
	if fake.lastRequest.TenantId != "acme" {
		t.Fatalf("TenantId = %q, want acme", fake.lastRequest.TenantId)
	}

	ctxB := authz.WithIdentity(context.Background(), authz.Identity{TenantID: "globex", Role: authz.RoleViewer})
	if _, err := client.Search(ctxB, "q", 5); err != nil {
		t.Fatalf("Search (globex): %v", err)
	}
	if fake.lastRequest.TenantId != "globex" {
		t.Fatalf("TenantId = %q, want globex", fake.lastRequest.TenantId)
	}
}
