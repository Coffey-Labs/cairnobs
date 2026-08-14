// Package searchclient is the tenant-scoped implementation of api's
// querylang/executor.SearchClient interface -- the Tantivy-side sibling
// of enterprise/internal/chrunner (see that package's doc comment for
// the shared reasoning: implementing a core interface structurally
// requires importing the package that defines it, which is why this
// lives in enterprise/ and imports api/, the allowed direction).
//
// Unlike chrunner, there is no separate "one connection per tenant"
// object here -- `search`'s gRPC service now holds the per-tenant
// registry itself (search/src/registry.rs), keyed by the
// SearchRequest.tenant_id field added in proto/sentry/search/v1/
// search.proto. This package's only job is resolving that field from
// the authenticated request identity before every call -- exactly the
// same "read from ctx, never a parameter, fail closed if absent" shape
// chrunner.Registry.RunSQL uses for ClickHouse.
package searchclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sentry/sentry/api/authz"
	searchv1 "github.com/sentry/sentry/proto/sentry/search/v1"
)

type Client struct {
	grpc searchv1.SearchServiceClient
	conn *grpc.ClientConn
}

// Dial mirrors api/searchclient.Dial exactly (same plain-TCP, no-TLS
// internal-service-to-service trust boundary) -- the only difference
// from that package is what Search does with the resolved tenant.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dialing search service at %s: %w", addr, err)
	}
	return &Client{grpc: searchv1.NewSearchServiceClient(conn), conn: conn}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Search implements executor.SearchClient. Resolves the caller's tenant
// from ctx (never a parameter) and fails closed -- no authenticated
// identity, or an identity with no tenant (RoleService, or a
// misconfigured authorizer), refuses the call rather than falling back
// to the single default index, which would silently defeat the whole
// point of this package existing. This mirrors chrunner.Registry.RunSQL's
// exact fail-closed shape.
func (c *Client) Search(ctx context.Context, query string, limit uint32) ([]string, error) {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("searchclient: no authenticated identity in context, refusing to search")
	}
	if identity.TenantID == "" {
		return nil, fmt.Errorf("searchclient: authenticated identity %q has no tenant, refusing to search", identity.Role)
	}

	resp, err := c.grpc.Search(ctx, &searchv1.SearchRequest{Query: query, Limit: limit, TenantId: identity.TenantID})
	if err != nil {
		return nil, err
	}
	return resp.GetRecordIds(), nil
}
