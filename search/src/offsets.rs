use anyhow::{Context, Result};
use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// Tracks per-partition offsets in a plain JSON file next to the Tantivy
/// index, since rskafka is a low-level client with no built-in consumer-
/// group coordination/offset-commit protocol (unlike kafka-go on the
/// ingest side) -- there's no broker-side group to commit to here, so
/// this service owns its own offset bookkeeping.
///
/// Best-effort, not exactly-once: if the process dies between processing
/// a record and persisting its offset, that record gets reprocessed on
/// restart. This is fine because `SearchIndex::upsert` is delete-then-add
/// on `record_id` -- reprocessing the same record overwrites the same
/// document rather than duplicating it.
#[derive(Debug)]
pub struct OffsetStore {
    path: PathBuf,
    offsets: HashMap<i32, i64>,
}

impl OffsetStore {
    pub async fn load(path: &Path) -> Result<Self> {
        let offsets = match tokio::fs::read(path).await {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .with_context(|| format!("parsing offsets file {}", path.display()))?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => HashMap::new(),
            Err(e) => return Err(e).with_context(|| format!("reading offsets file {}", path.display())),
        };
        Ok(Self {
            path: path.to_path_buf(),
            offsets,
        })
    }

    /// Next offset to fetch for a partition -- 0 (earliest) if never
    /// recorded before.
    pub fn get(&self, partition: i32) -> i64 {
        self.offsets.get(&partition).copied().unwrap_or(0)
    }

    pub fn set(&mut self, partition: i32, offset: i64) {
        self.offsets.insert(partition, offset);
    }

    pub async fn persist(&self) -> Result<()> {
        if let Some(parent) = self.path.parent() {
            tokio::fs::create_dir_all(parent)
                .await
                .with_context(|| format!("creating offsets directory {}", parent.display()))?;
        }
        let bytes = serde_json::to_vec_pretty(&self.offsets).context("serializing offsets")?;
        tokio::fs::write(&self.path, bytes)
            .await
            .with_context(|| format!("writing offsets file {}", self.path.display()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn get_defaults_to_zero_for_unknown_partition() {
        let dir = tempfile::tempdir().unwrap();
        let store = OffsetStore::load(&dir.path().join("offsets.json")).await.unwrap();
        assert_eq!(store.get(0), 0);
    }

    #[tokio::test]
    async fn missing_file_loads_as_empty_not_an_error() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("does-not-exist.json");
        let store = OffsetStore::load(&path).await;
        assert!(store.is_ok(), "expected a missing offsets file to load as empty, got {store:?}");
    }

    #[tokio::test]
    async fn persist_and_reload_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("offsets.json");

        let mut store = OffsetStore::load(&path).await.unwrap();
        store.set(0, 42);
        store.set(1, 7);
        store.persist().await.unwrap();

        let reloaded = OffsetStore::load(&path).await.unwrap();
        assert_eq!(reloaded.get(0), 42);
        assert_eq!(reloaded.get(1), 7);
        assert_eq!(reloaded.get(2), 0, "unset partition should still default to 0");
    }
}
