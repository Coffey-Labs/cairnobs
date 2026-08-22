mod batch;
mod config;
mod grpc;
mod metrics;
mod source;

#[cfg(windows)]
mod service;

pub mod pb {
    tonic::include_proto!("sentry.logs.v1");

    pub mod agent {
        pub mod v1 {
            tonic::include_proto!("sentry.agent.v1");
        }
    }
}

use anyhow::{Context, Result};
use batch::Batcher;
use clap::Parser;
use config::Config;
use pb::agent::v1::{agent_control_client::AgentControlClient, AgentCommand, CheckInRequest, DesiredOverride, ReportedConfig};
use pb::{log_ingest_client::LogIngestClient, LogRecord, Severity};
use std::collections::HashMap;
use std::collections::HashSet;
use std::path::PathBuf;
use std::time::Duration;
use tokio::sync::mpsc;
use tonic::transport::Channel;

#[derive(Parser)]
#[command(name = "cairnobs-agent", about = "Cairn OBS Linux/Windows log collector")]
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
    /// `cairnobs-agent` with no subcommand for a normal foreground/console
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

    // One long-lived channel for the whole process, not one per source
    // task -- both the primary source and every extra-file-path task
    // (see apply_override's extra_file_paths handling) send into clones
    // of the same `tx`. This matters specifically because the primary
    // source can be aborted and respawned at runtime (a journald_unit
    // override): if each respawn created a *new* channel the way this
    // used to work, any extra-file-path task still holding a clone of
    // the old `tx` would be silently orphaned (sending into a channel
    // whose `rx` had just been replaced/dropped). Creating the channel
    // once and only ever swapping the *task* (never the channel) avoids
    // that entirely.
    let (tx, mut rx) = mpsc::channel(1024);

    // source_cfg is mutable: a remote DesiredOverride's journald_unit
    // field (see apply_override below) can change it at runtime, which
    // means aborting and respawning the source task with the new
    // filter -- source_handle is mutable for the same reason.
    let mut source_cfg = cfg.source.clone();
    let mut source_handle = spawn_source_task(source_cfg.clone(), tx.clone());

    // Extra file paths an operator has remotely added via the web UI
    // (see apply_override's extra_file_paths handling) -- empty until
    // the first such override arrives. Keyed by path so a later
    // override with a different path list can be diffed against what's
    // already running: abort tasks for paths that were removed, spawn
    // tasks for paths that are new, leave everything else untouched.
    let mut extra_file_tasks: HashMap<PathBuf, tokio::task::JoinHandle<()>> = HashMap::new();

    let channel = grpc::connect(&cfg.ingest, &cfg.tls)
        .await
        .context("connecting to ingest service")?;
    let mut client = LogIngestClient::new(channel.clone());
    let mut control_client = AgentControlClient::new(channel);
    tracing::info!(endpoint = %cfg.ingest.endpoint, "connected to ingest service");

    // Effective runtime settings, seeded from local config -- every one
    // of these is mutable because a remote DesiredOverride can change
    // it (see apply_override). The local agent.toml is never rewritten;
    // an override lives only in memory here and reverts to agent.toml's
    // own values on restart, re-syncing on the next successful CheckIn
    // (see /docs/agent-management-design.md's merge-semantics section).
    let mut batch_max_size = cfg.batch.max_size;
    let mut flush_interval = Duration::from_millis(cfg.batch.flush_interval_ms);
    let mut heartbeat_enabled = cfg.heartbeat.enabled;
    let mut heartbeat_interval = cfg.heartbeat.interval;
    // Not remotely overridable (unlike batch/heartbeat above) -- see
    // /agent/README.md's Hosts-feature notes on why this is a
    // deliberate, per-host, config-file-only decision, not something
    // the web UI can flip on for an arbitrary agent.
    let metrics_enabled = cfg.metrics.enabled;
    let metrics_interval = cfg.metrics.interval;
    // Empty until the first override is ever applied -- echoed back on
    // every CheckIn as-is so the server can tell "pending" (an edit
    // exists this agent hasn't picked up) from "applied."
    let mut applied_override_version = String::new();

    let mut batcher = Batcher::new(batch_max_size, flush_interval);
    let mut ticker = tokio::time::interval(flush_interval.max(Duration::from_millis(50)));

    // Heartbeat's own ticker, independent of the batch flush ticker above
    // -- it always fires on heartbeat_interval regardless of the batch
    // settings or whether any real log traffic is flowing. Also drives
    // CheckIn (see the arm below) unconditionally -- CheckIn keeps
    // running even when heartbeat_enabled is false, since that's an
    // agent's only path to ever receive a remote override that
    // re-enables it; only the heartbeat log record itself is gated on
    // heartbeat_enabled.
    let mut heartbeat_ticker = tokio::time::interval(heartbeat_interval.max(Duration::from_millis(50)));

    // Unlike heartbeat_ticker, metrics has no secondary purpose (no
    // CheckIn-equivalent side effect) to keep it running when disabled,
    // so the select! arm below skips it entirely via `if metrics_enabled`
    // rather than always firing and conditionally acting.
    let mut metrics_ticker = tokio::time::interval(metrics_interval.max(Duration::from_millis(50)));

    loop {
        tokio::select! {
            _ = metrics_ticker.tick(), if metrics_enabled => {
                send_metrics(&mut client, &host, &service).await;
            }
            _ = heartbeat_ticker.tick() => {
                if heartbeat_enabled {
                    send_heartbeat(&mut client, &host, &service).await;
                }

                let reported = ReportedConfig {
                    agent_version: env!("CARGO_PKG_VERSION").to_string(),
                    source_kind: source_kind_name(&source_cfg),
                    source_detail: source_detail_summary(&source_cfg),
                    batch_max_size: batch_max_size as u64,
                    batch_flush_interval_ms: flush_interval.as_millis() as u64,
                    heartbeat_enabled,
                    heartbeat_interval_ms: heartbeat_interval.as_millis() as u64,
                };
                match grpc::check_in(&mut control_client, CheckInRequest {
                    host: host.clone(),
                    service: service.clone(),
                    current_config: Some(reported),
                    applied_override_version: applied_override_version.clone(),
                }).await {
                    Ok(resp) => {
                        if let Some(ov) = resp.has_override.then_some(resp.r#override).flatten() {
                            if ov.version != applied_override_version {
                                apply_override(
                                    &ov,
                                    &mut batch_max_size, &mut flush_interval,
                                    &mut heartbeat_enabled, &mut heartbeat_interval,
                                    &mut batcher, &mut ticker, &mut heartbeat_ticker,
                                    &mut source_cfg, &mut source_handle,
                                    &mut extra_file_tasks, &tx,
                                    &mut client,
                                ).await;
                                applied_override_version = ov.version.clone();
                                tracing::info!(version = %applied_override_version, "applied remote config override");
                            }
                        }
                        if AgentCommand::try_from(resp.pending_command) == Ok(AgentCommand::Restart) {
                            tracing::info!("received remote restart command, shutting down gracefully");
                            // flush_all(), not poll_timeout(): same reasoning as
                            // the normal shutdown path below -- whatever's
                            // buffered must go out regardless of whether
                            // flush_interval has elapsed yet.
                            if let Some(batch) = batcher.flush_all() {
                                flush(&mut client, batch).await;
                            }
                            source_handle.abort();
                            // A hard process exit, not a `break` out of this
                            // loop: this agent's own restart policy is
                            // entirely the host's service manager's
                            // responsibility (systemd/Windows SCM), the same
                            // contract any well-behaved service relies on --
                            // see AgentCommand's doc comment for why STOP/
                            // UNINSTALL need real per-platform work this
                            // doesn't.
                            std::process::exit(0);
                        }
                    }
                    // A failed check-in is not fatal -- same graceful-
                    // degradation posture as a failed heartbeat/batch
                    // flush: an agent management feature being
                    // unreachable must never stop log collection.
                    Err(e) => tracing::debug!(error = %e, "check-in failed"),
                }
            }
            maybe_line = rx.recv() => {
                let Some(raw) = maybe_line else {
                    tracing::warn!("source exited, flushing remaining batch and shutting down");
                    break;
                };
                let parsed = cairnobs_parser::parse(&raw.line);
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

    // flush_all(), not poll_timeout(): shutdown must send whatever's
    // buffered unconditionally -- poll_timeout() only drains once
    // flush_interval has elapsed, so anything buffered more recently
    // than that would otherwise be silently dropped on every graceful
    // shutdown that happens to land between flushes. Same reasoning
    // applies to a config hot-reload replacing this batcher outright
    // (see apply_override).
    if let Some(batch) = batcher.flush_all() {
        flush(&mut client, batch).await;
    }
    source_handle.abort();
    Ok(())
}

/// Applies a newly-received DesiredOverride to the agent's in-memory
/// runtime state -- see run_agent's CheckIn arm, and
/// /docs/agent-management-design.md's merge-semantics section for why
/// this never touches the local agent.toml file. Every field is
/// independently optional (unset = keep the current value); batch/
/// heartbeat settings always get their batcher/ticker rebuilt together
/// when *any* override arrives, for simplicity, rather than tracking
/// which specific field changed -- this only runs when a human edits an
/// agent's config from the web UI, not on a hot path, so the extra
/// timer/allocation churn doesn't matter.
#[allow(clippy::too_many_arguments)]
async fn apply_override(
    ov: &DesiredOverride,
    batch_max_size: &mut usize,
    flush_interval: &mut Duration,
    heartbeat_enabled: &mut bool,
    heartbeat_interval: &mut Duration,
    batcher: &mut Batcher,
    ticker: &mut tokio::time::Interval,
    heartbeat_ticker: &mut tokio::time::Interval,
    source_cfg: &mut config::SourceConfig,
    source_handle: &mut tokio::task::JoinHandle<()>,
    extra_file_tasks: &mut HashMap<PathBuf, tokio::task::JoinHandle<()>>,
    tx: &source::LineSender,
    client: &mut LogIngestClient<Channel>,
) {
    if let Some(v) = ov.batch_max_size {
        *batch_max_size = v as usize;
    }
    if let Some(v) = ov.batch_flush_interval_ms {
        *flush_interval = Duration::from_millis(v);
    }
    // Flush whatever the old batcher was holding before replacing it --
    // a hot-reload must never silently drop buffered-but-not-yet-due
    // records, same reasoning as shutdown's flush_all() above.
    if let Some(old) = batcher.flush_all() {
        flush(client, old).await;
    }
    *batcher = Batcher::new(*batch_max_size, *flush_interval);
    *ticker = tokio::time::interval((*flush_interval).max(Duration::from_millis(50)));

    if let Some(v) = ov.heartbeat_enabled {
        *heartbeat_enabled = v;
    }
    if let Some(v) = ov.heartbeat_interval_ms {
        *heartbeat_interval = Duration::from_millis(v);
    }
    *heartbeat_ticker = tokio::time::interval((*heartbeat_interval).max(Duration::from_millis(50)));

    // Only meaningful (and only ever sent by the server) when this
    // agent's local source is journald -- ignored otherwise, per
    // agent_control.proto's DesiredOverride.journald_unit comment.
    // Changing it means aborting and respawning the source task: unlike
    // batch/heartbeat, there's no way to change what journald::run is
    // tailing without restarting that task.
    if let Some(unit) = &ov.journald_unit {
        if let config::SourceConfig::Journald { unit: current_unit } = source_cfg {
            let new_unit = if unit.is_empty() { None } else { Some(unit.clone()) };
            if *current_unit != new_unit {
                *source_cfg = config::SourceConfig::Journald { unit: new_unit };
                source_handle.abort();
                *source_handle = spawn_source_task(source_cfg.clone(), tx.clone());
                tracing::info!(unit = ?unit, "applied remote journald unit override, restarted source");
            }
        }
    }

    // Extra file paths to tail alongside whatever the primary source
    // above is -- see agent_control.proto's DesiredOverride.
    // extra_file_paths comment. Diffed against what's already running
    // (extra_file_tasks' keys) rather than blindly tearing everything
    // down and respawning: an edit to, say, heartbeat_interval_ms must
    // never interrupt a file that's already being tailed and hasn't
    // changed. `ov.extra_file_paths` is the complete desired list every
    // time (never a partial patch -- see the proto field's own doc
    // comment), so anything not in it gets removed.
    let desired: HashSet<PathBuf> = ov.extra_file_paths.iter().map(PathBuf::from).collect();
    let current: HashSet<PathBuf> = extra_file_tasks.keys().cloned().collect();

    for removed in current.difference(&desired) {
        if let Some(handle) = extra_file_tasks.remove(removed) {
            handle.abort();
            tracing::info!(path = %removed.display(), "stopped tailing removed extra file path");
        }
    }
    for added in desired.difference(&current) {
        extra_file_tasks.insert(added.clone(), spawn_extra_file_task(added.clone(), tx.clone()));
        tracing::info!(path = %added.display(), "started tailing new extra file path");
    }
}

fn spawn_source_task(source_cfg: config::SourceConfig, tx: source::LineSender) -> tokio::task::JoinHandle<()> {
    tokio::spawn(spawn_source(source_cfg, tx))
}

/// Same shape as `spawn_source`'s own per-source-kind feature gating --
/// an extra file path is really just another `File` source, tailed
/// from the end (no `from_beginning` knob for these; matches the
/// primary `file` source's own sensible default), feeding into the
/// same shared channel every other source in this process uses.
#[cfg_attr(not(feature = "file-tail"), allow(unused_variables))]
fn spawn_extra_file_task(path: PathBuf, tx: source::LineSender) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        #[cfg(feature = "file-tail")]
        let result = source::file_tail::run(&path, false, tx).await;
        #[cfg(not(feature = "file-tail"))]
        let result: Result<(), anyhow::Error> =
            Err(anyhow::anyhow!("this build was compiled without the `file-tail` feature"));
        if let Err(e) = result {
            tracing::error!(error = %e, path = %path.display(), "extra file path exited with error");
        }
    })
}

fn source_kind_name(cfg: &config::SourceConfig) -> String {
    match cfg {
        config::SourceConfig::Journald { .. } => "journald".to_string(),
        config::SourceConfig::File { .. } => "file".to_string(),
        config::SourceConfig::EventLog { .. } => "eventlog".to_string(),
        config::SourceConfig::Etw { .. } => "etw".to_string(),
    }
}

fn source_detail_summary(cfg: &config::SourceConfig) -> String {
    match cfg {
        config::SourceConfig::Journald { unit } => unit.clone().unwrap_or_else(|| "(whole journal)".to_string()),
        config::SourceConfig::File { path, .. } => path.display().to_string(),
        config::SourceConfig::EventLog { channels } => channels.join(","),
        config::SourceConfig::Etw { providers } => providers.join(","),
    }
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

/// Sends a single synthetic record through the same `PushBatch` RPC and
/// mTLS identity as real log data -- no new proto message, no new ingest
/// code, no new ClickHouse schema. Bypasses `Batcher` (see the heartbeat
/// ticker's own comment above): a heartbeat that got queued behind
/// `batch.max_size` or `batch.flush_interval_ms` would defeat the point
/// of a punctual "still alive" signal. Distinguished from a real log
/// record purely by the `cairnobs.heartbeat` attribute -- `service` stays
/// the agent's real configured service so it doesn't pollute
/// service-based dashboards/faceting with a fake value. See
/// /docs/agent-heartbeat-monitoring.md for how an absence alert rule
/// turns a run of missed heartbeats into a notification.
async fn send_heartbeat(client: &mut LogIngestClient<Channel>, host: &str, service: &str) {
    let record = LogRecord {
        timestamp_unix_nano: now_unix_nanos(),
        host: host.to_string(),
        service: service.to_string(),
        severity: Severity::Info as i32,
        message: "agent heartbeat".to_string(),
        attributes: std::collections::HashMap::from([("cairnobs.heartbeat".to_string(), "true".to_string())]),
        record_id: String::new(),
    };
    match grpc::send_batch(client, format!("heartbeat-{}", batch_id()), vec![record]).await {
        Ok(_) => tracing::debug!(host, "heartbeat sent"),
        Err(e) => tracing::warn!(error = %e, host, "heartbeat send failed"),
    }
}

/// Same "no new proto, no new ingest code, no new ClickHouse schema"
/// shape as `send_heartbeat` above -- a metrics sample is just another
/// tagged `LogRecord`, distinguished by the `cairnobs.metrics` attribute.
/// Unlike heartbeat, the numeric fields themselves are real query-language
/// attributes too (`cpu_percent`, `mem_used_bytes`, etc.) rather than
/// being folded into `message` -- confirmed before building this that
/// arbitrary attribute names are transparently queryable/comparable as
/// numbers (`api/querylang/executor/sql.go`'s `topLevelFields` fallback
/// to `attributes['field']` with automatic numeric casting), so there's
/// no need to encode them as a JSON blob in `message` and parse it
/// client-side instead.
async fn send_metrics(client: &mut LogIngestClient<Channel>, host: &str, service: &str) {
    let m = match metrics::collect("/").await {
        Ok(m) => m,
        Err(e) => {
            tracing::warn!(error = %e, host, "collecting host metrics failed");
            return;
        }
    };
    let record = LogRecord {
        timestamp_unix_nano: now_unix_nanos(),
        host: host.to_string(),
        service: service.to_string(),
        severity: Severity::Info as i32,
        message: "host metrics".to_string(),
        attributes: std::collections::HashMap::from([
            ("cairnobs.metrics".to_string(), "true".to_string()),
            ("cpu_percent".to_string(), format!("{:.2}", m.cpu_percent)),
            ("mem_used_bytes".to_string(), m.mem_used_bytes.to_string()),
            ("mem_total_bytes".to_string(), m.mem_total_bytes.to_string()),
            ("disk_used_bytes".to_string(), m.disk_used_bytes.to_string()),
            ("disk_total_bytes".to_string(), m.disk_total_bytes.to_string()),
            // Static-or-slow-changing context, not utilization numbers --
            // sent on the same record so a viewer never has to correlate
            // two different samples to make sense of the numbers above
            // (see metrics::Metrics's doc comment).
            ("cpu_cores".to_string(), m.cpu_cores.to_string()),
            ("os_name".to_string(), m.os_name.clone()),
            ("kernel_version".to_string(), m.kernel_version.clone()),
            ("arch".to_string(), m.arch.to_string()),
            ("uptime_seconds".to_string(), m.uptime_seconds.to_string()),
            // Comma-joined -- LogRecord attributes are string-valued,
            // and a host can have more than one address per family
            // (multi-NIC, or a v6 privacy/temporary address alongside
            // the stable one). Empty string, not an omitted key, when
            // a host genuinely has none of a given family -- matches
            // every other soft-failed context field here (see
            // metrics::collect's unwrap_or_default for this one).
            ("ipv4_addresses".to_string(), m.ipv4_addresses.join(",")),
            ("ipv6_addresses".to_string(), m.ipv6_addresses.join(",")),
        ]),
        record_id: String::new(),
    };
    match grpc::send_batch(client, format!("metrics-{}", batch_id()), vec![record]).await {
        Ok(_) => tracing::debug!(host, "metrics sent"),
        Err(e) => tracing::warn!(error = %e, host, "metrics send failed"),
    }
}

fn now_unix_nanos() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as i64)
        .unwrap_or(0)
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
