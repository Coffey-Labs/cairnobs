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
    /// registry::IndexRegistry for both the read side (SearchRequest.
    /// tenant_id) and the write side (consumer.rs, routing on each
    /// record's tenant_id Kafka header) -- `index_path` above stays the
    /// single shared index an untagged record, or any deployment that
    /// never turned on ingest's TenantResolver, still lands in. Default
    /// matches the path convention deploy/operator's Tenant controller
    /// and enterprise/internal/rbacstore's seeded default data source
    /// already assume (`/var/lib/sentry-search/tenants/<id>`).
    pub tenants_index_path: PathBuf,
    /// Base URL of enterprise-auth's HTTP API, e.g.
    /// `http://enterprise-auth:8082` -- same env var name and "empty
    /// means off" shape as ingest/internal/config's own
    /// ENTERPRISE_AUTH_URL. When set (together with
    /// ENTERPRISE_AUTH_SERVICE_TOKEN below), tenants::ActiveTenantTracker
    /// gates consumer.rs's write-routing on a polled active-tenant
    /// allowlist -- see that module's doc comment for why this needed a
    /// network call instead of direct Postgres access. When unset,
    /// write-routing behaves exactly as it did before that tracker
    /// existed: any syntactically-valid tenant_id is trusted.
    pub enterprise_auth_url: Option<String>,
    /// RoleService Bearer credential this process presents to
    /// enterprise-auth's GET /internal/active-tenants -- minted via the
    /// existing `enterprise-auth -mint-service-token search` (the flag
    /// is already generic over caller name, no backend change needed to
    /// mint one for a new caller). Required together with
    /// enterprise_auth_url above; Config::load fails if exactly one of
    /// the two is set, rather than silently running with the tracker
    /// half-configured.
    pub enterprise_auth_service_token: Option<String>,
}

impl Config {
    pub fn load() -> Result<Self> {
        let commit_interval_ms: u64 = getenv("COMMIT_INTERVAL_MS", "2000")
            .parse()
            .context("COMMIT_INTERVAL_MS must be a number")?;

        let enterprise_auth_url = getenv_opt("ENTERPRISE_AUTH_URL");
        let enterprise_auth_service_token = getenv_opt("ENTERPRISE_AUTH_SERVICE_TOKEN");
        if enterprise_auth_url.is_some() != enterprise_auth_service_token.is_some() {
            anyhow::bail!(
                "ENTERPRISE_AUTH_URL and ENTERPRISE_AUTH_SERVICE_TOKEN must be set together, or neither -- got exactly one"
            );
        }

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
            enterprise_auth_url,
            enterprise_auth_service_token,
        })
    }
}

fn getenv(key: &str, fallback: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| fallback.to_string())
}

fn getenv_opt(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.is_empty())
}
