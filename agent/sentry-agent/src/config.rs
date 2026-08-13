use anyhow::{Context, Result};
use serde::Deserialize;
use std::path::{Path, PathBuf};

#[cfg(not(windows))]
const DEFAULT_CONFIG_PATH: &str = "/etc/sentry-agent/agent.toml";
#[cfg(windows)]
const DEFAULT_CONFIG_PATH: &str = r"C:\ProgramData\SentryAgent\agent.toml";

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(default)]
pub struct Config {
    pub agent: AgentConfig,
    pub source: SourceConfig,
    pub batch: BatchConfig,
    pub ingest: IngestConfig,
    pub tls: TlsConfig,
}

impl Config {
    /// Loads config from `explicit_path` if given, else from the
    /// platform's conventional config path if it exists
    /// (`/etc/sentry-agent/agent.toml` on Linux,
    /// `C:\ProgramData\SentryAgent\agent.toml` on Windows), else falls
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
    PathBuf::from(format!("/etc/sentry-agent/{name}"))
}

#[cfg(windows)]
fn default_cert_path(name: &str) -> PathBuf {
    PathBuf::from(format!(r"C:\ProgramData\SentryAgent\{name}"))
}
