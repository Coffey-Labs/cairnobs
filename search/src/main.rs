mod config;
mod consumer;
mod grpc;
mod index;
mod offsets;
mod registry;
mod tenants;

pub mod logsv1 {
    tonic::include_proto!("sentry.logs.v1");
}
pub mod searchv1 {
    tonic::include_proto!("cairnobs.search.v1");
}

use anyhow::{Context, Result};
use config::Config;
use index::SearchIndex;
use registry::IndexRegistry;
use std::sync::Arc;
use tonic::transport::Server;

/// Matches /transport/provision-topics.sh's default
/// REDPANDA_TOPIC_PARTITIONS -- documented cross-component contract, not
/// discovered dynamically. See consumer.rs.
const DEFAULT_PARTITION_COUNT: i32 = 6;

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cfg = Arc::new(Config::load().context("loading config")?);

    // Off unless ENTERPRISE_AUTH_URL/ENTERPRISE_AUTH_SERVICE_TOKEN are
    // both set (Config::load already rejects exactly one being set).
    // Blocks startup entirely on failure, same "fail hard, let the
    // orchestrator restart" posture enterprise-ingest's main.go already
    // uses when its own required startup fetch (rbacstore.
    // ListProvisionedDataSources) fails -- see tenants.rs's doc comment
    // for why a partial/degraded startup isn't the safer choice here.
    let active_tenants = match (&cfg.enterprise_auth_url, &cfg.enterprise_auth_service_token) {
        (Some(url), Some(token)) => {
            tracing::info!(url, "active-tenant write-routing gate enabled");
            Some(tenants::ActiveTenantTracker::start(url, token).await?)
        }
        _ => {
            tracing::info!("ENTERPRISE_AUTH_URL not set -- write-routing has no active-tenant gate");
            None
        }
    };

    let index = Arc::new(
        SearchIndex::open_or_create(&cfg.index_path).context("opening tantivy index")?,
    );
    // Per-tenant indices are resolved on demand by IndexRegistry, opened
    // under cfg.tenants_index_path -- see registry.rs's doc comment for
    // what this isolates (both read and write now) and the one residual
    // gap it doesn't close.
    let registry = Arc::new(IndexRegistry::new(Arc::clone(&index), cfg.tenants_index_path.clone()));

    let partition_count: i32 = std::env::var("REDPANDA_TOPIC_PARTITIONS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(DEFAULT_PARTITION_COUNT);

    let consumer_cfg = Arc::clone(&cfg);
    let consumer_registry = Arc::clone(&registry);
    let consumer_active_tenants = active_tenants.clone();
    let consumer_handle = tokio::spawn(async move {
        if let Err(e) = consumer::run(consumer_cfg, consumer_registry, partition_count, consumer_active_tenants).await {
            tracing::error!(error = %e, "redpanda consumer exited with error");
        }
    });

    let addr = cfg
        .grpc_listen_addr
        .parse()
        .context("parsing GRPC_LISTEN_ADDR")?;
    tracing::info!(addr = %cfg.grpc_listen_addr, "search gRPC server listening");

    let search_server = grpc::SearchServer::new(Arc::clone(&registry));
    Server::builder()
        .add_service(searchv1::search_service_server::SearchServiceServer::new(
            search_server,
        ))
        .serve(addr)
        .await
        .context("gRPC server failed")?;

    consumer_handle.abort();
    Ok(())
}
