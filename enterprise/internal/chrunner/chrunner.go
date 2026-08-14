// Package chrunner is the tenant-scoped implementation of api's
// querylang/executor.SQLRunner interface -- the piece
// /docs/security/threat-model.md's headline finding says was missing:
// until this package, api/cmd/api/main.go opened exactly one shared
// ClickHouse connection for every tenant, no matter how many
// tenant_memberships/Tenant CRs existed. This package requires
// importing api/querylang/executor and api/authz directly (see
// enterprise/go.mod's replace directive) -- implementing
// executor.SQLRunner structurally requires it (its RunSQL method
// returns *executor.Result, a type only that package defines), and
// that's the allowed import direction: enterprise -> api, never the
// reverse (hack/check-tenant-boundary.sh enforces that direction only).
//
// Design, per /docs/phase-4-isolation-design.md's ClickHouse section:
// Registry holds one fully separate *executor.ChRunner (and the
// driver.Conn under it) per tenant, built once at construction from an
// immutable map -- never a shared pool with session-level `USE`, which
// is a classic concurrency bug (a connection recycled between tenants
// mid-flight can interleave one tenant's session state into another's
// query). RunSQL resolves which tenant's runner to use from the
// request's authz.Identity (attached to ctx by
// api/authz.RequireRole/RequireRoleOrService), never from any
// caller-suppliable parameter -- there is no code path in this package
// that accepts a tenant ID as an argument to a query-executing method.
package chrunner

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/querylang/executor"
)

// DataSource is the minimal shape Registry needs to open one tenant's
// connection -- deliberately not enterprise/internal/rbacstore.DataSource
// itself, so this package doesn't need to import rbacstore just to
// describe "an address and a credential." Callers (enterprise-api's
// main.go) adapt rbacstore rows into this.
type DataSource struct {
	TenantID string
	Database string
	Username string
	Password string
}

// Registry implements executor.SQLRunner by routing each call to the
// caller's tenant-specific connection. Immutable after New returns --
// see this file's doc comment on why that's load-bearing, not just a
// style choice.
type Registry struct {
	runners map[string]*executor.ChRunner
	closers []func()
}

// New opens one real ClickHouse connection per DataSource (same native
// address for all of them -- tenants sharing a physical ClickHouse
// server today, per-tenant *pinning* to dedicated cluster nodes is
// named as later, non-schema-changing work in
// /docs/phase-4-isolation-design.md, not something this constructor
// does). Fails closed: if any one tenant's connection can't be opened
// or doesn't ping successfully, the whole Registry fails to construct
// rather than silently running with a partial tenant set -- a tenant
// missing from the map is a clear, loud "unknown tenant" error at query
// time (see RunSQL), not a connection nobody noticed never came up.
func New(ctx context.Context, addr string, sources []DataSource) (*Registry, error) {
	reg := &Registry{runners: make(map[string]*executor.ChRunner, len(sources))}
	for _, src := range sources {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{addr},
			Auth: clickhouse.Auth{
				Database: src.Database,
				Username: src.Username,
				Password: src.Password,
			},
		})
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("chrunner: opening connection for tenant %q: %w", src.TenantID, err)
		}
		if err := conn.Ping(ctx); err != nil {
			_ = conn.Close()
			reg.Close()
			return nil, fmt.Errorf("chrunner: pinging connection for tenant %q: %w", src.TenantID, err)
		}
		reg.runners[src.TenantID] = executor.NewChRunner(conn)
		reg.closers = append(reg.closers, func() { _ = conn.Close() })
	}
	return reg, nil
}

// Close releases every underlying connection -- call once at process
// shutdown, same lifecycle as the single conn.Close() api/cmd/api/main.go
// defers today, just fanned out over N connections.
func (r *Registry) Close() {
	for _, c := range r.closers {
		c()
	}
}

// RunSQL implements executor.SQLRunner. Resolves the caller's tenant
// from ctx (never a parameter -- see this file's doc comment) and fails
// closed on every ambiguous case: no identity, an identity with no
// tenant (RoleService, or a misconfigured authorizer), or a tenant with
// no provisioned connection all return an error, never a fallback to
// some other tenant's connection or an arbitrarily-chosen default.
func (r *Registry) RunSQL(ctx context.Context, sql string) (*executor.Result, error) {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("chrunner: no authenticated identity in context, refusing to run query")
	}
	if identity.TenantID == "" {
		return nil, fmt.Errorf("chrunner: authenticated identity %q has no tenant, refusing to run query", identity.Role)
	}
	runner, ok := r.runners[identity.TenantID]
	if !ok {
		return nil, fmt.Errorf("chrunner: tenant %q has no provisioned ClickHouse connection", identity.TenantID)
	}
	return runner.RunSQL(ctx, sql)
}
