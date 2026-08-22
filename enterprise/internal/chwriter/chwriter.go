// Package chwriter is enterprise/internal/chrunner's write-side
// counterpart -- the tenant-scoped implementation ingest/consumer.
// Consumer needs to route each batch's records into their own tenant's
// dedicated ClickHouse database, instead of the one shared table
// ingest/cmd/ingest's single-tenant mode always writes to. Requires
// importing ingest/clickhousewriter and ingest/consumer directly (see
// enterprise/go.mod's replace directive) -- same allowed
// "enterprise -> core" import direction chrunner uses for
// api/querylang/executor, just against a different core module.
//
// Design mirrors chrunner.Registry closely: one fully separate
// *clickhousewriter.Writer (and the driver.Conn under it) per tenant,
// built once at construction from an immutable map -- never a shared
// pool with session-level USE, for the same concurrency reasons
// chrunner's doc comment explains. The one real difference:
// chrunner.RunSQL resolves exactly one tenant per call from ctx (a
// single request always belongs to one identity); WriteBatch resolves
// per *record*, since one Kafka batch pulled off the shared
// sentry.logs.raw topic can freely mix records from many different
// tenants -- see ingest/internal/grpcserver's doc comment for why
// there's one shared topic, not topic-per-tenant.
package chwriter

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/ingest/clickhousewriter"
	"github.com/cairnobs/cairnobs/ingest/consumer"
	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// DataSource mirrors chrunner.DataSource -- deliberately not
// enterprise/internal/rbacstore.DataSource itself, so this package
// doesn't need to import rbacstore just to describe "an address and a
// credential." Callers (enterprise-ingest's main.go) adapt rbacstore
// rows into this.
type DataSource struct {
	TenantID string
	Database string
	Username string
	Password string
}

// Registry implements ingest/consumer's chWriter interface
// (WriteBatch(ctx, []consumer.Record) error) by routing each record to
// its tenant's dedicated connection. The writer map used to be
// immutable after New returned; StartRefreshing (below) makes it
// mutable at runtime, guarded by mu -- WriteBatch takes a read lock (the
// common case, and concurrent reads don't block each other), a refresh
// takes a write lock only for the brief final swap, never while
// actually dialing ClickHouse (see refresh's comment).
type Registry struct {
	addr string

	mu      sync.RWMutex
	writers map[string]*clickhousewriter.Writer
}

// New opens one real ClickHouse connection per DataSource (same native
// address for all of them, different per-tenant credentials -- tenants
// sharing a physical ClickHouse server today, same as chrunner). Fails
// closed: if any one tenant's connection can't be opened, the whole
// Registry fails to construct rather than silently running with a
// partial tenant set.
func New(ctx context.Context, addr string, sources []DataSource) (*Registry, error) {
	reg := &Registry{addr: addr, writers: make(map[string]*clickhousewriter.Writer, len(sources))}
	for _, src := range sources {
		w, err := clickhousewriter.New(ctx, clickhousewriter.Config{
			Addr: addr, Database: src.Database, Username: src.Username, Password: src.Password,
		})
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("chwriter: opening connection for tenant %q: %w", src.TenantID, err)
		}
		reg.writers[src.TenantID] = w
	}
	return reg, nil
}

// Close releases every underlying connection -- call once at process
// shutdown, same lifecycle as chrunner.Registry.Close. Safe to call
// even with StartRefreshing's goroutine still running (it only ever
// adds/removes individual writers under mu, never assumes the whole map
// survives), though callers should still cancel that goroutine's
// context first to stop it from reopening what Close just shut down.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.writers {
		_ = w.Close()
	}
}

// WriteBatch implements ingest/consumer's chWriter interface. Groups
// records by TenantID and writes each tenant's group through its own
// dedicated connection -- fails the *whole* call (matching
// ingest/consumer's existing all-or-nothing batch contract: a failed
// WriteBatch means no offsets are committed and the entire batch is
// redelivered, never partial credit) if any record's tenant is empty
// (no TenantResolver was configured for the PushBatch call that
// produced it -- a multi-tenant deployment must never silently write an
// untagged record somewhere) or unrecognized (not yet provisioned, or
// provisioning failed). Fail closed, same reasoning
// chrunner.Registry.RunSQL's doc comment gives for the read side.
//
// A permanently-unprovisioned or permanently-mistagged tenant would
// stall this consumer's offset progress entirely (every redelivery of
// that batch fails the same way) -- a real, disclosed limitation of
// reusing ingest/consumer's existing all-or-nothing contract rather
// than building new partial-batch-success semantics nothing else in
// this codebase has either. See /docs/phase-4-runbook.md.
func (r *Registry) WriteBatch(ctx context.Context, records []consumer.Record) error {
	byTenant := make(map[string][]consumer.Record, len(records))
	for _, rec := range records {
		byTenant[rec.TenantID] = append(byTenant[rec.TenantID], rec)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for tenantID, group := range byTenant {
		if tenantID == "" {
			return fmt.Errorf("chwriter: %d record(s) in this batch have no tenant_id, refusing to write any of it", len(group))
		}
		writer, ok := r.writers[tenantID]
		if !ok {
			return fmt.Errorf("chwriter: tenant %q has no provisioned ClickHouse connection, refusing to write %d record(s)", tenantID, len(group))
		}
		plain := make([]*logsv1.LogRecord, len(group))
		for i, rec := range group {
			plain[i] = rec.Record
		}
		if err := writer.WriteBatch(ctx, plain); err != nil {
			return fmt.Errorf("chwriter: writing batch for tenant %q: %w", tenantID, err)
		}
	}
	return nil
}

// SourceLister re-lists the data sources a Registry should have a
// writer for -- a narrow function type, not an rbacstore dependency,
// same reasoning DataSource's doc comment gives for not importing
// rbacstore directly here. enterprise-ingest's main.go supplies one
// backed by rbacstore.ListProvisionedDataSources (the same query New's
// caller already runs once at startup).
type SourceLister func(ctx context.Context) ([]DataSource, error)

// StartRefreshing closes the staleness gap disclosed in
// /docs/security/threat-model.md as an asymmetry with search's
// tenants.ActiveTenantTracker (Tantivy's write-side active-tenant gate,
// which already refreshes every 60s): spawns a goroutine that
// periodically re-lists data sources via lister and reconciles the
// writer map -- opens a connection for any newly-active tenant, closes
// and removes any tenant no longer present (deprovisioned or suspended
// since the last refresh). Stops when ctx is cancelled; call at most
// once per Registry. A refresh failure (lister error, or one tenant's
// new connection failing to open) logs via logger and leaves the
// existing map alone for that tick -- a transient rbacstore/Postgres
// blip, or one bad tenant's connection, must not evict every other
// tenant's already-working writer, the same "last-known-good" posture
// ActiveTenantTracker's periodic refresh uses.
func (r *Registry) StartRefreshing(ctx context.Context, lister SourceLister, interval time.Duration, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.refresh(ctx, lister, logger)
			}
		}
	}()
}

// refresh dials any newly-needed connections *before* taking the write
// lock, so a slow/unreachable ClickHouse for one newly-active tenant
// never blocks WriteBatch's read lock for longer than the map swap
// itself takes.
func (r *Registry) refresh(ctx context.Context, lister SourceLister, logger *slog.Logger) {
	sources, err := lister(ctx)
	if err != nil {
		logger.Error("chwriter: refreshing data sources failed, keeping last-known-good writer set", "error", err)
		return
	}
	fresh := make(map[string]DataSource, len(sources))
	for _, src := range sources {
		fresh[src.TenantID] = src
	}

	r.mu.RLock()
	var toOpen []DataSource
	for tenantID, src := range fresh {
		if _, ok := r.writers[tenantID]; !ok {
			toOpen = append(toOpen, src)
		}
	}
	r.mu.RUnlock()

	newWriters := make(map[string]*clickhousewriter.Writer, len(toOpen))
	for _, src := range toOpen {
		w, err := clickhousewriter.New(ctx, clickhousewriter.Config{
			Addr: r.addr, Database: src.Database, Username: src.Username, Password: src.Password,
		})
		if err != nil {
			logger.Error("chwriter: opening connection for newly-active tenant failed, will retry next refresh", "tenant_id", src.TenantID, "error", err)
			continue
		}
		newWriters[src.TenantID] = w
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for tenantID, w := range newWriters {
		r.writers[tenantID] = w
		logger.Info("chwriter: added writer for newly-active tenant", "tenant_id", tenantID)
	}
	for tenantID, w := range r.writers {
		if _, ok := fresh[tenantID]; !ok {
			_ = w.Close()
			delete(r.writers, tenantID)
			logger.Info("chwriter: removed writer for tenant no longer active/provisioned", "tenant_id", tenantID)
		}
	}
}
