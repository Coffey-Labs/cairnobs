package groundingregistry

import (
	"context"
	"testing"

	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/querylang/executor"
)

// tenantAwareFakeRunner returns a service list keyed by the tenant
// identity RunSQL is called with -- confirms groundingregistry actually
// stamps a different tenant per Service.Refresh call, not the same
// context reused for everyone (chrunner.Registry's real RunSQL resolves
// tenant from ctx the same way, so this fake exercises the same
// contract).
type tenantAwareFakeRunner struct {
	byTenant map[string][]string
}

func (f *tenantAwareFakeRunner) RunSQL(ctx context.Context, sql string) (*executor.Result, error) {
	id, ok := authz.IdentityFromContext(ctx)
	if !ok {
		return &executor.Result{Columns: []string{"service"}, Rows: nil}, nil
	}
	services := f.byTenant[id.TenantID]
	rows := make([][]any, len(services))
	for i, s := range services {
		rows[i] = []any{s}
	}
	return &executor.Result{Columns: []string{"service"}, Rows: rows}, nil
}

func TestRefreshAllScopesEachTenantIndependently(t *testing.T) {
	runner := &tenantAwareFakeRunner{byTenant: map[string][]string{
		"tenant-a": {"api-a"},
		"tenant-b": {"api-b", "worker-b"},
	}}
	reg := New(runner)
	lister := func(context.Context) ([]string, error) {
		return []string{"tenant-a", "tenant-b"}, nil
	}

	reg.refreshAll(context.Background(), lister, nil)

	a := reg.SchemaContextFor("tenant-a")
	if len(a.Services) != 1 || a.Services[0] != "api-a" {
		t.Errorf("tenant-a grounding = %v, want [api-a]", a.Services)
	}
	b := reg.SchemaContextFor("tenant-b")
	if len(b.Services) != 2 {
		t.Errorf("tenant-b grounding = %v, want 2 services", b.Services)
	}

	unknown := reg.SchemaContextFor("tenant-never-seen")
	if len(unknown.Services) != 0 || len(unknown.Fields) != 0 {
		t.Errorf("unseen tenant should get a zero-valued SchemaContext, got %+v", unknown)
	}
}

func TestRefreshAllListerErrorLeavesExistingSnapshots(t *testing.T) {
	runner := &tenantAwareFakeRunner{byTenant: map[string][]string{"tenant-a": {"api-a"}}}
	reg := New(runner)
	good := func(context.Context) ([]string, error) { return []string{"tenant-a"}, nil }
	reg.refreshAll(context.Background(), good, nil)

	failing := func(context.Context) ([]string, error) { return nil, context.DeadlineExceeded }
	reg.refreshAll(context.Background(), failing, nil)

	a := reg.SchemaContextFor("tenant-a")
	if len(a.Services) != 1 {
		t.Errorf("tenant-a grounding after a failed lister call = %v, want unchanged [api-a]", a.Services)
	}
}
