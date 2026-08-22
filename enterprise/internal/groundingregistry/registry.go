// Package groundingregistry gives each active tenant its own schema-
// grounding snapshot (Phase 7 task 3) in a multi-tenant deployment,
// mirroring enterprise/internal/chwriter.Registry's per-tenant-instance
// shape. It exists because api/ai/grounding.Service is deliberately
// tenant-agnostic (it just wraps whatever executor.SQLRunner it's given
// and caches one snapshot) -- a multi-tenant deployment needs many
// snapshots, one per tenant, refreshed independently.
//
// The underlying SQLRunner every tenant's Service samples through is the
// *same* chrunner.Registry instance shared across all of them: chrunner
// resolves which tenant's actual ClickHouse connection to use from the
// context.Context passed to RunSQL, not from anything this package
// stores per tenant -- see chrunner.Registry.RunSQL's doc comment. So
// "one grounding.Service per tenant" doesn't mean one ClickHouse
// connection per tenant here (chrunner already owns that); it means one
// cached snapshot per tenant, refreshed by calling that tenant's
// Service.Refresh with a context stamped with that tenant's identity via
// api/authz.WithIdentity -- the same "construct our own request context
// outside an HTTP handler" pattern that function's doc comment names
// this exact kind of caller as being for.
package groundingregistry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/api/ai/grounding"
	"github.com/cairnobs/cairnobs/api/ai/provider"
	"github.com/cairnobs/cairnobs/api/authz"
	"github.com/cairnobs/cairnobs/api/querylang/executor"
)

// TenantLister returns the currently-active tenant IDs to sample --
// a narrow function type rather than an rbacstore dependency, same
// reasoning chwriter.Registry's SourceLister gives: this package
// shouldn't need to import rbacstore just to know its return type.
// enterprise-api's main.go supplies one backed by
// rbacstore.ListProvisionedDataSources, the same source chrunner/
// chwriter's own registries already refresh from.
type TenantLister func(ctx context.Context) ([]string, error)

// Registry holds one grounding.Service per active tenant, all sharing
// the same underlying SQLRunner (chrunner.Registry).
type Registry struct {
	runner executor.SQLRunner

	mu       sync.RWMutex
	services map[string]*grounding.Service
}

func New(runner executor.SQLRunner) *Registry {
	return &Registry{runner: runner, services: make(map[string]*grounding.Service)}
}

// SchemaContextFor returns tenant's cached grounding snapshot, or a
// zero-valued SchemaContext if that tenant hasn't been sampled yet (new
// tenant, not yet seen by a refresh cycle) -- same "absence is normal,
// not an error" posture grounding.Service.Current documents.
func (r *Registry) SchemaContextFor(tenantID string) provider.SchemaContext {
	r.mu.RLock()
	svc, ok := r.services[tenantID]
	r.mu.RUnlock()
	if !ok {
		return provider.SchemaContext{}
	}
	return svc.Current()
}

// SchemaContext implements aiapi.SchemaContextSource, resolving the
// tenant from ctx the same way chrunner.RunSQL does -- the multi-tenant
// counterpart to grounding.Service's own same-named method, which has
// no tenant to resolve in a single-tenant deployment. An unauthenticated
// or tenant-less context (shouldn't happen behind aiapi's RoleViewer
// auth wrapper, but handled rather than assumed) returns a zero-valued
// SchemaContext, same as an unseen tenant -- absence is normal here, not
// worth a panic or a swallowed error over.
func (r *Registry) SchemaContext(ctx context.Context) provider.SchemaContext {
	id, ok := authz.IdentityFromContext(ctx)
	if !ok || id.TenantID == "" {
		return provider.SchemaContext{}
	}
	return r.SchemaContextFor(id.TenantID)
}

// StartRefreshing lists active tenants and refreshes each one's
// grounding snapshot, immediately and then on interval, until ctx is
// cancelled -- same shape as chwriter.Registry.StartRefreshing. A
// newly-active tenant gets a Service the first time it appears in
// lister's output; a tenant that's no longer listed keeps its last
// snapshot rather than being torn down (grounding data going briefly
// stale for a deprovisioned tenant is harmless -- unlike a ClickHouse
// writer connection, there's no credential to leak or clean up here).
func (r *Registry) StartRefreshing(ctx context.Context, lister TenantLister, interval time.Duration, logger *slog.Logger) {
	r.refreshAll(ctx, lister, logger)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.refreshAll(ctx, lister, logger)
			}
		}
	}()
}

func (r *Registry) refreshAll(ctx context.Context, lister TenantLister, logger *slog.Logger) {
	tenantIDs, err := lister(ctx)
	if err != nil {
		if logger != nil {
			logger.Error("groundingregistry: listing active tenants", "error", err)
		}
		return
	}
	for _, tenantID := range tenantIDs {
		svc := r.serviceFor(tenantID)
		tenantCtx := authz.WithIdentity(ctx, authz.Identity{TenantID: tenantID, Role: authz.RoleService})
		if err := svc.Refresh(tenantCtx); err != nil && logger != nil {
			logger.Error("groundingregistry: refreshing tenant", "tenant", tenantID, "error", err)
		}
	}
}

func (r *Registry) serviceFor(tenantID string) *grounding.Service {
	r.mu.RLock()
	svc, ok := r.services[tenantID]
	r.mu.RUnlock()
	if ok {
		return svc
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if svc, ok := r.services[tenantID]; ok { // re-check under write lock
		return svc
	}
	svc = grounding.New(r.runner)
	r.services[tenantID] = svc
	return svc
}
