use anyhow::{bail, Context, Result};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::RwLock;

use crate::index::SearchIndex;

/// Resolves a `tenant_id` to its own `SearchIndex`, opening one on
/// demand under `<tenants_root>/<tenant_id>` the first time it's
/// requested. Used on both sides now: the read side
/// (`SearchRequest.tenant_id`, per search.proto's doc comment -- set
/// only by a trusted server-side caller, `enterprise/internal/
/// searchclient`, from the authenticated request identity, never a
/// value a browser/client controls directly) and the write side
/// (`consumer.rs`, from a Kafka message's `tenant_id` header, attached
/// server-side by `ingest/internal/grpcserver` after validating an
/// agent's per-tenant credential -- never a value the agent's message
/// body controls directly either).
///
/// An empty `tenant_id` resolves to `default_index` -- the single index
/// path every Phase 0-3 deployment, and every untagged record, still
/// uses.
///
/// `resolve` itself still has no active-tenant gate of its own -- it
/// will happily open-or-create an index for any syntactically-valid
/// `tenant_id`, active or not. That's deliberate: this struct's job is
/// managing index lifecycles, not policy, the same separation
/// `clickhousewriter.Writer` (mechanism) vs. `chwriter.Registry`
/// (policy: which tenants get a writer at all) draws on the ClickHouse
/// side. The gate lives one layer up, at each caller:
/// `enterprise/internal/searchclient.TenantChecker` for the read side
/// (refuses to even issue a search for a tenant that isn't `active` in
/// `rbacstore`, via a direct Postgres-backed query since that code runs
/// in `enterprise/`), and `consumer.rs`'s `tenants::ActiveTenantTracker`
/// for the write side (a polled allowlist fetched from a new
/// `enterprise-auth` endpoint over HTTP -- `search` is AGPL core with no
/// Postgres access and no `enterprise/` import allowed, so it needed a
/// network boundary instead of an import one, the same shape
/// `ingest/internal/grpcserver.TenantResolver` already uses against the
/// same service). Both gates are optional at this layer -- `resolve`
/// itself works identically whether or not either caller happens to
/// gate it -- so a future caller that forgets to gate would silently
/// reopen this exact class of gap; see `consumer.rs`'s call site for
/// the write side's enforcement.
pub struct IndexRegistry {
    default_index: Arc<SearchIndex>,
    tenants_root: PathBuf,
    tenants: RwLock<HashMap<String, Arc<SearchIndex>>>,
}

impl IndexRegistry {
    pub fn new(default_index: Arc<SearchIndex>, tenants_root: PathBuf) -> Self {
        Self {
            default_index,
            tenants_root,
            tenants: RwLock::new(HashMap::new()),
        }
    }

    /// Resolves (opening on first use) the index for `tenant_id`, or the
    /// default index when `tenant_id` is empty.
    pub async fn resolve(&self, tenant_id: &str) -> Result<Arc<SearchIndex>> {
        if tenant_id.is_empty() {
            return Ok(Arc::clone(&self.default_index));
        }

        {
            let tenants = self.tenants.read().await;
            if let Some(idx) = tenants.get(tenant_id) {
                return Ok(Arc::clone(idx));
            }
        }

        validate_tenant_id(tenant_id)?;

        // Two concurrent first-requests for the same never-before-seen
        // tenant could both reach here -- resolved by re-checking under
        // the write lock via `entry().or_insert_with(..)` below, so at
        // most one SearchIndex ever actually gets constructed and
        // stored, even if both callers did the (cheap, idempotent)
        // open_or_create call.
        let path = self.tenants_root.join(tenant_id);
        let opened = SearchIndex::open_or_create(&path)
            .with_context(|| format!("opening tantivy index for tenant {tenant_id:?}"))?;

        let mut tenants = self.tenants.write().await;
        let idx = tenants
            .entry(tenant_id.to_string())
            .or_insert_with(|| Arc::new(opened));
        Ok(Arc::clone(idx))
    }

    /// Commits `default_index` plus every tenant index opened so far --
    /// the periodic-commit ticker in consumer.rs calls this instead of
    /// committing a single index, now that a batch of records can span
    /// several tenants' indices. An index that was never opened (no
    /// write ever routed to it) is never touched, matching `resolve`'s
    /// own on-demand-open behavior -- nothing to commit for a tenant
    /// with no traffic yet. One tenant's commit failing does not stop
    /// the others from being attempted -- a single broken index
    /// shouldn't stall every other tenant's documents from becoming
    /// searchable. Returns the last error encountered, if any, after
    /// every index has been tried.
    pub async fn commit_all(&self) -> Result<()> {
        let mut last_err = self.default_index.commit().await.context("committing default index").err();
        let tenants = self.tenants.read().await;
        for (tenant_id, idx) in tenants.iter() {
            if let Err(e) = idx
                .commit()
                .await
                .with_context(|| format!("committing index for tenant {tenant_id:?}"))
            {
                tracing::error!(error = %e, tenant_id, "failed to commit tenant tantivy index");
                last_err = Some(e);
            }
        }
        match last_err {
            Some(e) => Err(e),
            None => Ok(()),
        }
    }
}

/// Mirrors enterprise/internal/tenantprovision's tenantIdentifierPattern
/// (Go) exactly -- tenant_id becomes a literal filesystem path component
/// here, the same class of injection concern that package's doc comment
/// explains for ClickHouse DDL identifiers.
fn validate_tenant_id(tenant_id: &str) -> Result<()> {
    let mut chars = tenant_id.chars();
    let starts_ok = chars.next().is_some_and(|c| c.is_ascii_lowercase());
    let rest_ok = chars.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '_');
    if !starts_ok || !rest_ok || tenant_id.len() > 63 {
        bail!("tenant_id {tenant_id:?} is not a safe index-directory name");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn new_test_registry() -> (IndexRegistry, Arc<SearchIndex>, tempfile::TempDir) {
        let dir = tempfile::tempdir().expect("creating temp dir");
        let default_index =
            Arc::new(SearchIndex::open_or_create(&dir.path().join("default")).unwrap());
        let registry = IndexRegistry::new(Arc::clone(&default_index), dir.path().join("tenants"));
        (registry, default_index, dir)
    }

    #[tokio::test]
    async fn empty_tenant_id_resolves_to_default_index() {
        let (registry, default_index, _dir) = new_test_registry();
        let resolved = registry.resolve("").await.unwrap();
        assert!(Arc::ptr_eq(&resolved, &default_index));
    }

    #[tokio::test]
    async fn same_tenant_id_resolves_to_the_same_index_instance() {
        let (registry, _default, _dir) = new_test_registry();
        let a = registry.resolve("acme").await.unwrap();
        let b = registry.resolve("acme").await.unwrap();
        assert!(Arc::ptr_eq(&a, &b), "expected the same Arc<SearchIndex> on a second resolve");
    }

    #[tokio::test]
    async fn different_tenants_resolve_to_different_index_instances() {
        let (registry, _default, _dir) = new_test_registry();
        let a = registry.resolve("acme").await.unwrap();
        let b = registry.resolve("globex").await.unwrap();
        assert!(!Arc::ptr_eq(&a, &b), "expected different tenants to get different index instances");
    }

    #[tokio::test]
    async fn tenant_index_is_isolated_from_default_and_other_tenants() {
        let (registry, _default, _dir) = new_test_registry();

        let default_idx = registry.resolve("").await.unwrap();
        default_idx.upsert("default-1", "shared term").await.unwrap();
        default_idx.commit().await.unwrap();

        let acme_idx = registry.resolve("acme").await.unwrap();
        acme_idx.upsert("acme-1", "shared term").await.unwrap();
        acme_idx.commit().await.unwrap();

        let globex_idx = registry.resolve("globex").await.unwrap();
        globex_idx.upsert("globex-1", "shared term").await.unwrap();
        globex_idx.commit().await.unwrap();

        // The core adversarial probe from
        // /docs/phase-4-isolation-design.md's verification plan, item 3:
        // a search scoped to one tenant must never return another
        // tenant's (or the default index's) matching documents, even
        // though the term exists in all three.
        assert_eq!(acme_idx.search("shared", 10).unwrap(), vec!["acme-1"]);
        assert_eq!(globex_idx.search("shared", 10).unwrap(), vec!["globex-1"]);
        assert_eq!(default_idx.search("shared", 10).unwrap(), vec!["default-1"]);
    }

    #[tokio::test]
    async fn rejects_unsafe_tenant_id() {
        let (registry, _default, _dir) = new_test_registry();
        for bad in ["", "../etc", "Acme", "has spaces", "-leading-dash"] {
            if bad.is_empty() {
                continue; // empty is valid -- resolves to the default index, not an error
            }
            assert!(
                registry.resolve(bad).await.is_err(),
                "expected {bad:?} to be rejected as an unsafe tenant_id"
            );
        }
    }

    #[tokio::test]
    async fn concurrent_first_resolves_for_the_same_new_tenant_share_one_instance() {
        let (registry, _default, _dir) = new_test_registry();
        let registry = Arc::new(registry);

        let mut handles = Vec::new();
        for _ in 0..8 {
            let registry = Arc::clone(&registry);
            handles.push(tokio::spawn(async move { registry.resolve("acme").await.unwrap() }));
        }
        let mut results = Vec::new();
        for h in handles {
            results.push(h.await.unwrap());
        }
        for r in &results[1..] {
            assert!(Arc::ptr_eq(&results[0], r), "expected every concurrent resolve to return the same Arc<SearchIndex>");
        }
    }

    #[tokio::test]
    async fn commit_all_commits_default_and_every_opened_tenant_index() {
        let (registry, default_index, _dir) = new_test_registry();

        default_index.upsert("default-1", "hello").await.unwrap();
        let acme_idx = registry.resolve("acme").await.unwrap();
        acme_idx.upsert("acme-1", "hello").await.unwrap();
        let globex_idx = registry.resolve("globex").await.unwrap();
        globex_idx.upsert("globex-1", "hello").await.unwrap();

        // Nothing committed yet -- none of the three should be
        // searchable, proving this test would actually catch commit_all
        // silently skipping an index rather than passing vacuously.
        assert!(default_index.search("hello", 10).unwrap().is_empty());
        assert!(acme_idx.search("hello", 10).unwrap().is_empty());
        assert!(globex_idx.search("hello", 10).unwrap().is_empty());

        registry.commit_all().await.unwrap();

        assert_eq!(default_index.search("hello", 10).unwrap(), vec!["default-1"]);
        assert_eq!(acme_idx.search("hello", 10).unwrap(), vec!["acme-1"]);
        assert_eq!(globex_idx.search("hello", 10).unwrap(), vec!["globex-1"]);
    }

    #[tokio::test]
    async fn commit_all_is_a_noop_for_tenants_never_resolved() {
        // A tenant with no traffic yet has no directory created at all --
        // commit_all must not try to open/commit anything for it.
        let (registry, _default, dir) = new_test_registry();
        registry.commit_all().await.unwrap();
        assert!(!dir.path().join("tenants").join("never-seen").exists());
    }
}
