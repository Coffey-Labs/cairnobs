use anyhow::{Context, Result};
use std::path::PathBuf;
use std::time::Duration;

/// All via environment variables, same convention as /ingest and /api —
/// no config file format for this service either.
pub struct Config {
    pub grpc_listen_addr: String,
    pub redpanda_brokers: Vec<String>,
    pub redpanda_topic: String,
    pub index_path: PathBuf,
    pub offsets_path: PathBuf,
    pub commit_interval: Duration,
    /// Phase 4: per-tenant index directories live under here, one
    /// subdirectory per tenant_id, opened on demand by
    /// registry::IndexRegistry -- distinct from `index_path` above,
    /// which stays the single shared index every ingest-written record
    /// lands in regardless of tenant (see registry.rs's doc comment and
    /// /docs/security/threat-model.md's ingest-tenancy caveat). Default
    /// matches the path convention deploy/operator's Tenant controller
    /// and enterprise/internal/rbacstore's seeded default data source
    /// already assume (`/var/lib/sentry-search/tenants/<id>`).
    pub tenants_index_path: PathBuf,
}

impl Config {
    pub fn load() -> Result<Self> {
        let commit_interval_ms: u64 = getenv("COMMIT_INTERVAL_MS", "2000")
            .parse()
            .context("COMMIT_INTERVAL_MS must be a number")?;

        Ok(Self {
            // Rust's SocketAddr parser needs a full address, unlike Go's
            // net package (ingest/api's ":PORT" convention won't parse
            // here).
            grpc_listen_addr: getenv("GRPC_LISTEN_ADDR", "0.0.0.0:50052"),
            redpanda_brokers: getenv("REDPANDA_BROKERS", "localhost:9092")
                .split(',')
                .map(str::to_string)
                .collect(),
            redpanda_topic: getenv("REDPANDA_TOPIC", "sentry.logs.raw"),
            index_path: PathBuf::from(getenv("INDEX_PATH", "/var/lib/sentry-search/index")),
            offsets_path: PathBuf::from(getenv(
                "OFFSETS_PATH",
                "/var/lib/sentry-search/offsets.json",
            )),
            commit_interval: Duration::from_millis(commit_interval_ms),
            tenants_index_path: PathBuf::from(getenv(
                "TENANTS_INDEX_PATH",
                "/var/lib/sentry-search/tenants",
            )),
        })
    }
}

fn getenv(key: &str, fallback: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| fallback.to_string())
}
