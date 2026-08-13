use anyhow::{Context, Result};
use serde::Deserialize;
use std::path::{Path, PathBuf};

const DEFAULT_CONFIG_PATH: &str = "/etc/sentry-agent/agent.toml";

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
    /// Loads config from `explicit_path` if given, else from
    /// `/etc/sentry-agent/agent.toml` if it exists, else falls back to
    /// built-in defaults (journald source, default TLS cert paths). Only an
    /// explicitly-passed `--config` path that doesn't exist is an error;
    /// the conventional default path is optional.
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
        unit: Option<String>,
    },
    File {
        path: PathBuf,
        #[serde(default)]
        from_beginning: bool,
    },
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
            ca_cert: PathBuf::from("/etc/sentry-agent/ca.pem"),
            client_cert: PathBuf::from("/etc/sentry-agent/client.pem"),
            client_key: PathBuf::from("/etc/sentry-agent/client-key.pem"),
        }
    }
}
