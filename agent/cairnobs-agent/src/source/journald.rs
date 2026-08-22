use super::{LineSender, RawLine};
use anyhow::{Context, Result};
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;

/// Reads journald entries by shelling out to `journalctl -f -o json`
/// rather than linking libsystemd via FFI. Linking libsystemd into a
/// statically-linked musl binary is fragile (it pulls in dbus/libcap
/// transitively and isn't designed for static linking) and would work
/// against the no-glibc-runtime-deps constraint in spirit even where it's
/// technically possible. `journalctl` ships on every systemd distro this
/// agent targets, so shelling out avoids the problem entirely. See
/// /docs/architecture.md.
pub async fn run(unit: Option<&str>, tx: LineSender) -> Result<()> {
    let mut cmd = Command::new("journalctl");
    cmd.arg("-f")
        .arg("-o")
        .arg("json")
        .arg("--since=now")
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null());
    if let Some(unit) = unit {
        cmd.arg("-u").arg(unit);
    }

    let mut child = cmd
        .spawn()
        .context("spawning journalctl -f -o json (is systemd-journal installed?)")?;
    let stdout = child.stdout.take().context("journalctl child had no stdout")?;
    let mut lines = BufReader::new(stdout).lines();

    while let Some(line) = lines.next_line().await.context("reading journalctl output")? {
        let Ok(entry) = serde_json::from_str::<serde_json::Value>(&line) else {
            tracing::warn!(%line, "skipping unparseable journalctl JSON line");
            continue;
        };

        let message = entry
            .get("MESSAGE")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        if message.is_empty() {
            continue;
        }

        let severity_hint = entry
            .get("PRIORITY")
            .and_then(|v| v.as_str().map(str::to_string).or_else(|| v.as_u64().map(|n| n.to_string())))
            .and_then(|s| s.parse::<u8>().ok())
            .filter(|&p| p <= 7);

        let timestamp_unix_nano = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos() as i64)
            .unwrap_or(0);

        if tx
            .send(RawLine {
                line: message,
                timestamp_unix_nano,
                severity_hint,
                extra_attributes: Default::default(),
            })
            .await
            .is_err()
        {
            break; // receiver dropped, agent is shutting down
        }
    }

    let status = child.wait().await.context("waiting for journalctl to exit")?;
    tracing::warn!(?status, "journalctl exited");
    Ok(())
}
