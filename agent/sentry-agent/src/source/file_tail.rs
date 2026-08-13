use super::{LineSender, RawLine};
use anyhow::{Context, Result};
use std::io::SeekFrom;
use std::path::Path;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::fs::File;
use tokio::io::{AsyncBufReadExt, AsyncSeekExt, BufReader};

const POLL_INTERVAL: Duration = Duration::from_millis(500);

/// Polling-based file tailer: no inotify/`notify` crate dependency. Good
/// enough for Phase 0 (journald is the primary source). Handles basic
/// truncation (e.g. logrotate `copytruncate`) by detecting the file shrank
/// and reopening from the start. Does not follow rename-based rotation
/// (logrotate `create`) — that's deferred until file-tail is more than a
/// fallback path.
pub async fn run(path: &Path, from_beginning: bool, tx: LineSender) -> Result<()> {
    let file = File::open(path)
        .await
        .with_context(|| format!("opening {}", path.display()))?;

    let mut pos = if from_beginning { 0 } else { file.metadata().await?.len() };

    let mut reader = BufReader::new(file);
    reader.seek(SeekFrom::Start(pos)).await?;
    let mut buf = String::new();

    loop {
        buf.clear();
        let n = reader
            .read_line(&mut buf)
            .await
            .context("reading line from file")?;
        if n == 0 {
            let metadata = tokio::fs::metadata(path).await.context("stat-ing file")?;
            if metadata.len() < pos {
                tracing::warn!(path = %path.display(), "file shrank, assuming truncation and reopening from start");
                let f = File::open(path)
                    .await
                    .context("reopening file after truncation")?;
                reader = BufReader::new(f);
                pos = 0;
            }
            tokio::time::sleep(POLL_INTERVAL).await;
            continue;
        }
        pos += n as u64;

        let line = buf.trim_end_matches(['\n', '\r']).to_string();
        if line.is_empty() {
            continue;
        }

        let timestamp_unix_nano = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos() as i64)
            .unwrap_or(0);

        if tx
            .send(RawLine {
                line,
                timestamp_unix_nano,
                severity_hint: None,
                extra_attributes: Default::default(),
            })
            .await
            .is_err()
        {
            break;
        }
    }

    Ok(())
}
