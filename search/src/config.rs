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
        })
    }
}

fn getenv(key: &str, fallback: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| fallback.to_string())
}
