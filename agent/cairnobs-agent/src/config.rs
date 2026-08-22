use anyhow::{Context, Result};
use serde::{Deserialize, Deserializer};
use std::path::{Path, PathBuf};
use std::time::Duration;

#[cfg(not(windows))]
const DEFAULT_CONFIG_PATH: &str = "/etc/cairnobs-agent/agent.toml";
#[cfg(windows)]
const DEFAULT_CONFIG_PATH: &str = r"C:\ProgramData\CairnObsAgent\agent.toml";

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(default)]
pub struct Config {
    pub agent: AgentConfig,
    pub source: SourceConfig,
    pub batch: BatchConfig,
    pub heartbeat: HeartbeatConfig,
    pub metrics: MetricsConfig,
    pub ingest: IngestConfig,
    pub tls: TlsConfig,
}

impl Config {
    /// Loads config from `explicit_path` if given, else from the
    /// platform's conventional config path if it exists
    /// (`/etc/cairnobs-agent/agent.toml` on Linux,
    /// `C:\ProgramData\CairnObsAgent\agent.toml` on Windows), else falls
    /// back to built-in defaults (journald source on Linux, default TLS
    /// cert paths). Only an explicitly-passed `--config` path that doesn't
    /// exist is an error; the conventional default path is optional.
    pub fn load(explicit_path: Option<&Path>) -> Result<Config> {
        let path = match explicit_path {
            Some(p) => Some(p.to_path_buf()),
            None => {
                let default = PathBuf::from(DEFAULT_CONFIG_PATH);
                default.exists().then_some(default)
            }
        };

        match path {
            Some(p) => {
                let raw = std::fs::read_to_string(&p)
                    .with_context(|| format!("reading config file {}", p.display()))?;
                toml::from_str(&raw).with_context(|| format!("parsing config file {}", p.display()))
            }
            None => Ok(Config::default()),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct AgentConfig {
    /// Overrides the auto-detected system hostname. Defaults to reading
    /// /etc/hostname at startup when unset.
    pub host: Option<String>,
    pub service: String,
}

impl Default for AgentConfig {
    fn default() -> Self {
        Self {
            host: None,
            service: "default".to_string(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "lowercase", tag = "kind")]
pub enum SourceConfig {
    Journald {
        #[serde(default)]
        #[cfg_attr(not(all(feature = "journald", target_os = "linux")), allow(dead_code))]
        unit: Option<String>,
    },
    // Dead-code-on-default-build, same reasoning as EventLog/Etw below:
    // these fields are only read by the `file-tail`-gated arm in
    // spawn_source (main.rs), which doesn't exist in the default build
    // (`default = ["journald"]`). Pre-existing gap from Phase 0 — CLAUDE.md
    // mandates plain `cargo clippy --all-targets -- -D warnings` (no
    // --all-features), which this broke silently since only
    // --all-features clippy was ever actually run.
    File {
        #[cfg_attr(not(feature = "file-tail"), allow(dead_code))]
        path: PathBuf,
        #[serde(default)]
        #[cfg_attr(not(feature = "file-tail"), allow(dead_code))]
        from_beginning: bool,
    },
    /// Windows Event Log via EvtSubscribe. Requires the agent to be built
    /// with the `windows-eventlog` feature; see /agent/README.md.
    ///
    /// `channels`/`providers` below are read only by the Windows-only
    /// consumers in `spawn_source` (main.rs), which don't exist at all on
    /// non-Windows builds — unlike `File`'s fields (dead only when the
    /// `file-tail` feature happens to be off), these are dead on *every*
    /// non-Windows build regardless of feature flags, since their sole
    /// consumer is `target_os = "windows"`-gated. `cfg_attr` here keeps
    /// clippy honest: still flags genuine dead code on an actual Windows
    /// build, just not on the platform where these fields can never be
    /// read no matter what.
    EventLog {
        #[serde(default = "default_eventlog_channels")]
        #[cfg_attr(not(windows), allow(dead_code))]
        channels: Vec<String>,
    },
    /// ETW (Event Tracing for Windows). Requires the `etw` feature and
    /// (usually) elevated privileges — see /agent/README.md before
    /// enabling this in any environment that isn't Windows-first.
    Etw {
        #[cfg_attr(not(windows), allow(dead_code))]
        providers: Vec<String>,
    },
}

fn default_eventlog_channels() -> Vec<String> {
    vec![
        "Application".to_string(),
        "System".to_string(),
        "Security".to_string(),
    ]
}

impl Default for SourceConfig {
    fn default() -> Self {
        SourceConfig::Journald { unit: None }
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct BatchConfig {
    pub max_size: usize,
    pub flush_interval_ms: u64,
}

impl Default for BatchConfig {
    fn default() -> Self {
        Self {
            max_size: 500,
            flush_interval_ms: 2000,
        }
    }
}

/// Sent independently of `batch` -- a heartbeat is a punctual liveness
/// signal, not log data, so it bypasses `Batcher` entirely (see
/// main.rs's `send_heartbeat`) rather than waiting on `max_size`/
/// `flush_interval_ms` like real records do. This is the operator-facing
/// "polling resolution" knob: how often this agent proves it's still
/// alive, which a `condition_type = "absence"` alert rule on the
/// `cairnobs.heartbeat` attribute (see /docs/agent-heartbeat-monitoring.md)
/// turns into "alert when this host goes quiet."
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct HeartbeatConfig {
    pub enabled: bool,
    #[serde(deserialize_with = "deserialize_duration")]
    pub interval: Duration,
}

impl Default for HeartbeatConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            interval: Duration::from_secs(60),
        }
    }
}

/// Off by default -- same "off unless configured" posture as every other
/// optional feature in this codebase -- since collecting host metrics is
/// a deliberate per-host decision (see /agent/README.md's Hosts-feature
/// notes: only one agent process per physical host should have this on,
/// to avoid duplicate/fragmented metric series when several agent
/// processes share a host under different `[agent] host` overrides).
/// Sent independently of `batch`, same reasoning and mechanism as
/// `HeartbeatConfig` above (see main.rs's `send_metrics`).
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct MetricsConfig {
    pub enabled: bool,
    #[serde(deserialize_with = "deserialize_duration")]
    pub interval: Duration,
}

impl Default for MetricsConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            interval: Duration::from_secs(60),
        }
    }
}

/// Parses a human-friendly duration string with an explicit unit suffix
/// -- "30s", "5m", "1h" -- deliberately the same s/m/h vocabulary
/// `earliest=`/`latest=` use in the query language
/// (/docs/query-language-reference.md), so the interval you set here and
/// the window you write in the matching alert rule's query read the same
/// way. Kept as a small hand-rolled parser rather than pulling in a
/// duration-parsing crate for this one field -- this is the
/// statically-linked edge agent every "no glibc runtime deps" constraint
/// in CLAUDE.md is about keeping lean, and the grammar needed here is a
/// handful of lines.
fn deserialize_duration<'de, D>(deserializer: D) -> Result<Duration, D::Error>
where
    D: Deserializer<'de>,
{
    let s = String::deserialize(deserializer)?;
    parse_duration(&s).map_err(serde::de::Error::custom)
}

fn parse_duration(s: &str) -> Result<Duration, String> {
    let s = s.trim();
    let (num, unit) = s.split_at(s.len().saturating_sub(1));
    let n: u64 = num
        .parse()
        .map_err(|_| format!("expected a duration like \"30s\", \"5m\", or \"1h\", got {s:?}"))?;
    match unit {
        "s" => Ok(Duration::from_secs(n)),
        "m" => Ok(Duration::from_secs(n * 60)),
        "h" => Ok(Duration::from_secs(n * 3600)),
        _ => Err(format!("expected a time unit of s, m, or h after {n}, got {s:?}")),
    }
}

#[cfg(test)]
mod heartbeat_config_tests {
    use super::*;

    #[test]
    fn parses_seconds_minutes_hours() {
        assert_eq!(parse_duration("30s").unwrap(), Duration::from_secs(30));
        assert_eq!(parse_duration("5m").unwrap(), Duration::from_secs(300));
        assert_eq!(parse_duration("2h").unwrap(), Duration::from_secs(7200));
    }

    #[test]
    fn rejects_missing_or_unknown_unit() {
        assert!(parse_duration("30").is_err());
        assert!(parse_duration("30x").is_err());
        assert!(parse_duration("").is_err());
    }

    #[test]
    fn default_is_60_seconds_and_enabled() {
        let cfg = HeartbeatConfig::default();
        assert!(cfg.enabled);
        assert_eq!(cfg.interval, Duration::from_secs(60));
    }

    #[test]
    fn toml_field_parses_via_deserialize() {
        #[derive(Deserialize)]
        struct Wrapper {
            #[serde(default)]
            heartbeat: HeartbeatConfig,
        }
        let w: Wrapper = toml::from_str("[heartbeat]\nenabled = true\ninterval = \"90s\"\n").unwrap();
        assert_eq!(w.heartbeat.interval, Duration::from_secs(90));
    }
}

#[cfg(test)]
mod metrics_config_tests {
    use super::*;

    #[test]
    fn default_is_60_seconds_and_disabled() {
        let cfg = MetricsConfig::default();
        assert!(!cfg.enabled);
        assert_eq!(cfg.interval, Duration::from_secs(60));
    }

    #[test]
    fn toml_field_parses_via_deserialize() {
        #[derive(Deserialize)]
        struct Wrapper {
            #[serde(default)]
            metrics: MetricsConfig,
        }
        let w: Wrapper = toml::from_str("[metrics]\nenabled = true\ninterval = \"30s\"\n").unwrap();
        assert!(w.metrics.enabled);
        assert_eq!(w.metrics.interval, Duration::from_secs(30));
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct IngestConfig {
    pub endpoint: String,
}

impl Default for IngestConfig {
    fn default() -> Self {
        Self {
            endpoint: "https://127.0.0.1:4317".to_string(),
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct TlsConfig {
    pub ca_cert: PathBuf,
    pub client_cert: PathBuf,
    pub client_key: PathBuf,
}

impl Default for TlsConfig {
    fn default() -> Self {
        Self {
            ca_cert: default_cert_path("ca.pem"),
            client_cert: default_cert_path("client.pem"),
            client_key: default_cert_path("client-key.pem"),
        }
    }
}

#[cfg(not(windows))]
fn default_cert_path(name: &str) -> PathBuf {
    PathBuf::from(format!("/etc/cairnobs-agent/{name}"))
}

#[cfg(windows)]
fn default_cert_path(name: &str) -> PathBuf {
    PathBuf::from(format!(r"C:\ProgramData\CairnObsAgent\{name}"))
}
