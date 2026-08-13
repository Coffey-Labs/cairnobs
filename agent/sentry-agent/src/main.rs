mod batch;
mod config;
mod grpc;
mod source;

pub mod pb {
    tonic::include_proto!("sentry.logs.v1");
}

use anyhow::{Context, Result};
use batch::Batcher;
use clap::Parser;
use config::Config;
use pb::{log_ingest_client::LogIngestClient, LogRecord, Severity};
use std::path::PathBuf;
use std::time::Duration;
use tokio::sync::mpsc;
use tonic::transport::Channel;

#[derive(Parser)]
#[command(name = "sentry-agent", about = "Sentry Linux log collector")]
struct Cli {
    /// Path to a TOML config file. Defaults to /etc/sentry-agent/agent.toml
    /// if present, otherwise built-in defaults (journald source, default
    /// TLS cert paths under /etc/sentry-agent/).
    #[arg(long)]
    config: Option<PathBuf>,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cli = Cli::parse();
    let cfg = Config::load(cli.config.as_deref()).context("loading config")?;

    let host = cfg.agent.host.clone().unwrap_or_else(default_hostname);
    let service = cfg.agent.service.clone();

    let (tx, mut rx) = mpsc::channel(1024);
    let source_handle = tokio::spawn(spawn_source(cfg.source.clone(), tx));

    let mut client = grpc::connect(&cfg.ingest, &cfg.tls)
        .await
        .context("connecting to ingest service")?;
    tracing::info!(endpoint = %cfg.ingest.endpoint, "connected to ingest service");

    let flush_interval = Duration::from_millis(cfg.batch.flush_interval_ms);
    let mut batcher = Batcher::new(cfg.batch.max_size, flush_interval);
    let mut ticker = tokio::time::interval(flush_interval.max(Duration::from_millis(50)));

    loop {
        tokio::select! {
            maybe_line = rx.recv() => {
                let Some(raw) = maybe_line else {
                    tracing::warn!("source exited, flushing remaining batch and shutting down");
                    break;
                };
                let parsed = sentry_parser::parse(&raw.line);
                let severity = to_pb_severity(raw.severity_hint.or(parsed.severity));
                let record = LogRecord {
                    timestamp_unix_nano: raw.timestamp_unix_nano,
                    host: host.clone(),
                    service: service.clone(),
                    severity: severity as i32,
                    message: parsed.message,
                    attributes: parsed.attributes.into_iter().collect(),
                };
                if let Some(batch) = batcher.push(record) {
                    flush(&mut client, batch).await;
                }
            }
            _ = ticker.tick() => {
                if let Some(batch) = batcher.poll_timeout() {
                    flush(&mut client, batch).await;
                }
            }
        }
    }

    if let Some(batch) = batcher.poll_timeout() {
        flush(&mut client, batch).await;
    }
    source_handle.abort();
    Ok(())
}

async fn spawn_source(source: config::SourceConfig, tx: source::LineSender) {
    let result = match source {
        #[cfg(feature = "journald")]
        config::SourceConfig::Journald { unit } => source::journald::run(unit.as_deref(), tx).await,
        #[cfg(not(feature = "journald"))]
        config::SourceConfig::Journald { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `journald` feature"))
        }

        #[cfg(feature = "file-tail")]
        config::SourceConfig::File { path, from_beginning } => {
            source::file_tail::run(&path, from_beginning, tx).await
        }
        #[cfg(not(feature = "file-tail"))]
        config::SourceConfig::File { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `file-tail` feature"))
        }
    };
    if let Err(e) = result {
        tracing::error!(error = %e, "log source exited with error");
    }
}

async fn flush(client: &mut LogIngestClient<Channel>, batch: Vec<LogRecord>) {
    let n = batch.len();
    let batch_id = batch_id();
    match grpc::send_batch(client, batch_id, batch).await {
        Ok(accepted) => tracing::debug!(accepted, sent = n, "batch flushed"),
        Err(e) => tracing::error!(error = %e, sent = n, "batch flush failed"),
    }
}

/// Best-effort batch identifier for ingest-side dedup on retry. Not
/// globally unique (host + nanosecond timestamp), which is good enough for
/// Phase 0's single-agent-per-host reality; revisit if agents ever share
/// an identity or clock resolution becomes a problem.
fn batch_id() -> String {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    format!("{nanos:x}")
}

fn to_pb_severity(sev: Option<u8>) -> Severity {
    match sev {
        Some(0..=2) => Severity::Fatal,       // emerg / alert / crit
        Some(3) => Severity::Error,           // err
        Some(4) => Severity::Warn,            // warning
        Some(5) | Some(6) => Severity::Info,  // notice / info
        Some(7) => Severity::Debug,           // debug
        _ => Severity::Unspecified,
    }
}

fn default_hostname() -> String {
    if let Ok(s) = std::fs::read_to_string("/etc/hostname") {
        let s = s.trim().to_string();
        if !s.is_empty() {
            return s;
        }
    }
    std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown-host".to_string())
}
