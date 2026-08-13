mod config;
mod consumer;
mod grpc;
mod index;
mod offsets;

pub mod logsv1 {
    tonic::include_proto!("sentry.logs.v1");
}
pub mod searchv1 {
    tonic::include_proto!("sentry.search.v1");
}

use anyhow::{Context, Result};
use config::Config;
use index::SearchIndex;
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

    let index = Arc::new(
        SearchIndex::open_or_create(&cfg.index_path).context("opening tantivy index")?,
    );

    let partition_count: i32 = std::env::var("REDPANDA_TOPIC_PARTITIONS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(DEFAULT_PARTITION_COUNT);

    let consumer_cfg = Arc::clone(&cfg);
    let consumer_index = Arc::clone(&index);
    let consumer_handle = tokio::spawn(async move {
        if let Err(e) = consumer::run(consumer_cfg, consumer_index, partition_count).await {
            tracing::error!(error = %e, "redpanda consumer exited with error");
        }
    });

    let addr = cfg
        .grpc_listen_addr
        .parse()
        .context("parsing GRPC_LISTEN_ADDR")?;
    tracing::info!(addr = %cfg.grpc_listen_addr, "search gRPC server listening");

    let search_server = grpc::SearchServer::new(Arc::clone(&index));
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
