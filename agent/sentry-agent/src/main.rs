mod batch;
mod config;
mod grpc;
mod source;

#[cfg(windows)]
mod service;

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
#[command(name = "sentry-agent", about = "Sentry Linux/Windows log collector")]
struct Cli {
    /// Path to a TOML config file. Defaults to the platform's conventional
    /// path if present, otherwise built-in defaults — see config::Config::load.
    #[arg(long)]
    config: Option<PathBuf>,

    #[cfg(windows)]
    #[command(subcommand)]
    command: Option<WindowsCommand>,
}

#[cfg(windows)]
#[derive(clap::Subcommand)]
enum WindowsCommand {
    /// Registers this binary as a Windows service (Automatic start,
    /// LocalSystem account). Requires an administrator shell.
    Install,
    /// Removes the Windows service registration.
    Uninstall,
    /// Entry point the Service Control Manager invokes when starting the
    /// registered service. Not meant to be run directly by a user — use
    /// `sentry-agent` with no subcommand for a normal foreground/console
    /// run, same as on Linux.
    RunService,
}

/// Not `#[tokio::main]`: the Windows service dispatcher
/// (`service_dispatcher::start`, see service.rs) is a blocking, synchronous
/// FFI call into the Service Control Manager and needs to be invoked
/// directly from a plain thread, not from inside an already-running tokio
/// runtime. Every other path builds its own runtime explicitly instead.
fn main() -> Result<()> {
    let cli = Cli::parse();

    #[cfg(windows)]
    {
        match cli.command {
            Some(WindowsCommand::Install) => return service::install().context("installing Windows service"),
            Some(WindowsCommand::Uninstall) => return service::uninstall().context("removing Windows service"),
            Some(WindowsCommand::RunService) => return service::run_as_service().context("running as a Windows service"),
            None => {}
        }
    }

    let rt = tokio::runtime::Runtime::new().context("building tokio runtime")?;
    rt.block_on(run_agent(cli.config))
}

/// The actual agent: load config, connect to ingest, run the source ->
/// parse -> batch -> ship loop until the source exits or the process is
/// signaled to stop. Called from `main()` directly for a normal run, and
/// from within the Windows service's own thread when running as a
/// service (see service.rs) — same logic either way.
pub async fn run_agent(config_path: Option<PathBuf>) -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cfg = Config::load(config_path.as_deref()).context("loading config")?;

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
                let mut attributes: std::collections::HashMap<String, String> =
                    parsed.attributes.into_iter().collect();
                // Source-provided structured fields (e.g. Windows Event
                // Log's EventID/Provider/Channel) win over anything the
                // RFC 5424 parser inferred from the raw text, since they
                // come from a more authoritative place.
                attributes.extend(raw.extra_attributes);
                let record = LogRecord {
                    timestamp_unix_nano: raw.timestamp_unix_nano,
                    host: host.clone(),
                    service: service.clone(),
                    severity: severity as i32,
                    message: parsed.message,
                    attributes,
                    // Always empty as sent by the agent -- ingest assigns
                    // this server-side. See the proto field comment.
                    record_id: String::new(),
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

// `tx` genuinely goes unused in one rare-but-valid combination: Windows
// features enabled while targeting a non-Windows platform (e.g. sanity-
// checking the Windows source arms compile shape from Linux, which is
// exactly how these were checked before a real Windows toolchain was
// available) collapses every arm to the tx-free `Err(...)` fallback.
#[allow(unused_variables)]
async fn spawn_source(source: config::SourceConfig, tx: source::LineSender) {
    // Explicit type: with an unusual feature combination (e.g.
    // windows-eventlog enabled while targeting Linux), every arm below can
    // collapse to the same untyped `Err(...)` fallback, and Rust can't
    // infer the Ok type without at least one real `.await` call anywhere
    // in the compiled match to anchor it.
    let result: Result<(), anyhow::Error> = match source {
        #[cfg(all(feature = "journald", target_os = "linux"))]
        config::SourceConfig::Journald { unit } => source::journald::run(unit.as_deref(), tx).await,
        #[cfg(not(all(feature = "journald", target_os = "linux")))]
        config::SourceConfig::Journald { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `journald` feature (or isn't targeting Linux)"))
        }

        #[cfg(feature = "file-tail")]
        config::SourceConfig::File { path, from_beginning } => {
            source::file_tail::run(&path, from_beginning, tx).await
        }
        #[cfg(not(feature = "file-tail"))]
        config::SourceConfig::File { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `file-tail` feature"))
        }

        #[cfg(all(feature = "windows-eventlog", target_os = "windows"))]
        config::SourceConfig::EventLog { channels } => source::windows_eventlog::run(&channels, tx).await,
        #[cfg(not(all(feature = "windows-eventlog", target_os = "windows")))]
        config::SourceConfig::EventLog { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `windows-eventlog` feature (or isn't targeting Windows)"))
        }

        #[cfg(all(feature = "etw", target_os = "windows"))]
        config::SourceConfig::Etw { providers } => source::etw::run(&providers, tx).await,
        #[cfg(not(all(feature = "etw", target_os = "windows")))]
        config::SourceConfig::Etw { .. } => {
            Err(anyhow::anyhow!("this build was compiled without the `etw` feature (or isn't targeting Windows)"))
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

#[cfg(not(windows))]
fn default_hostname() -> String {
    if let Ok(s) = std::fs::read_to_string("/etc/hostname") {
        let s = s.trim().to_string();
        if !s.is_empty() {
            return s;
        }
    }
    std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown-host".to_string())
}

#[cfg(windows)]
fn default_hostname() -> String {
    // Windows sets this in every process's environment; no Win32 API call
    // needed (GetComputerNameW would be the "proper" way, but this is the
    // same value and far simpler).
    std::env::var("COMPUTERNAME").unwrap_or_else(|_| "unknown-host".to_string())
}
