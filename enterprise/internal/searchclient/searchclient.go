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
//
// One real divergence from chrunner, found while closing
// /docs/phase-4-isolation-design.md's verification-plan item 4 (a
// mid-provisioning tenant must be refused, not served): search/src/
// registry.rs's IndexRegistry opens-or-creates an index for *any*
// syntactically-valid tenant_id on first request -- it has no concept of
// "is this tenant actually provisioned," because it's a separate
// process with no Postgres access, so it structurally can't know.
// chrunner gets its fail-closed property for free (a tenant not yet
// active+credentialed is simply absent from the immutable map
// enterprise-api's main.go builds at startup from
// rbacstore.ListProvisionedDataSources) -- Tantivy has no equivalent
// startup-time gate, so without a check *here*, a query against a
// mid-provisioning (or entirely made-up) tenant would silently succeed
// with zero results from a freshly-created empty index, rather than
// refusing -- "ambient success" masquerading as "no matching logs,"
// exactly the failure mode the verification plan named. TenantChecker
// closes that: Search now refuses before the gRPC call ever goes out if
// the tenant isn't active in rbacstore.
package searchclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sentry/sentry/api/authz"
	searchv1 "github.com/sentry/sentry/proto/sentry/search/v1"
)

// TenantChecker answers "is this tenant allowed to search at all" --
// backed by rbacstore.Store.TenantIsActive in production (a narrow
// interface, not *rbacstore.Store directly, so this package doesn't
// need rbacstore's full surface and tests can fake it without a live
// Postgres). Never cached: SetTenantStatus's doc comment already
// established "every tenant-resolution path elsewhere must re-check
// this via GetTenant, never cache/assume 'active'" for chrunner-style
// resolution, and the same reasoning applies here.
type TenantChecker interface {
	TenantIsActive(ctx context.Context, tenantID string) (bool, error)
}

type Client struct {
	grpc    searchv1.SearchServiceClient
	conn    *grpc.ClientConn
	tenants TenantChecker
}

// Dial mirrors api/searchclient.Dial exactly (same plain-TCP, no-TLS
// internal-service-to-service trust boundary) aside from the added
// TenantChecker -- the only difference from that package is what Search
// does with the resolved tenant. tenants is required, not optional:
// every production caller of this package is enterprise-api, which
// always has a live rbacstore.Store to pass (unlike, say,
// authz.Authorizer, there is no legitimate deployment shape where this
// package runs without one).
func Dial(addr string, tenants TenantChecker) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dialing search service at %s: %w", addr, err)
	}
	return &Client{grpc: searchv1.NewSearchServiceClient(conn), conn: conn, tenants: tenants}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Search implements executor.SearchClient. Resolves the caller's tenant
// from ctx (never a parameter) and fails closed -- no authenticated
// identity, an identity with no tenant (RoleService, or a misconfigured
// authorizer), or a tenant that isn't active in rbacstore all refuse the
// call rather than reaching `search`, which would otherwise silently
// open-or-create a fresh empty index for a tenant that was never
// actually provisioned (see this file's package doc comment). This
// mirrors chrunner.Registry.RunSQL's exact fail-closed shape, just with
// an explicit check where chrunner gets the same guarantee for free from
// its immutable connection map.
func (c *Client) Search(ctx context.Context, query string, limit uint32) ([]string, error) {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("searchclient: no authenticated identity in context, refusing to search")
	}
	if identity.TenantID == "" {
		return nil, fmt.Errorf("searchclient: authenticated identity %q has no tenant, refusing to search", identity.Role)
	}
	active, err := c.tenants.TenantIsActive(ctx, identity.TenantID)
	if err != nil {
		return nil, fmt.Errorf("searchclient: checking tenant %q status: %w", identity.TenantID, err)
	}
	if !active {
		return nil, fmt.Errorf("searchclient: tenant %q is not active, refusing to search", identity.TenantID)
	}

	resp, err := c.grpc.Search(ctx, &searchv1.SearchRequest{Query: query, Limit: limit, TenantId: identity.TenantID})
	if err != nil {
		return nil, err
	}
	return resp.GetRecordIds(), nil
}
