use anyhow::{Context, Result};
use std::path::Path;
use tantivy::collector::TopDocs;
use tantivy::query::QueryParser;
use tantivy::schema::{Schema, Value, STORED, STRING, TEXT};
use tantivy::{doc, Index, IndexReader, IndexWriter, ReloadPolicy, TantivyDocument, Term};
use tokio::sync::Mutex;

/// Minimal Tantivy index: a stable `record_id` (stored, exact-match) and
/// tokenized `message` text. Everything else (timestamp, host, service,
/// severity) is fetched by joining `record_id` back against ClickHouse in
/// `/api`'s search handler, not duplicated in here — this stays a pure
/// text index, not a second copy of the row.
pub struct SearchIndex {
    index: Index,
    writer: Mutex<IndexWriter>,
    reader: IndexReader,
    record_id_field: tantivy::schema::Field,
    message_field: tantivy::schema::Field,
}

/// 50MB is Tantivy's own suggested minimum writer heap budget; Phase 1
/// has no real sizing data yet to tune this against.
const WRITER_HEAP_BYTES: usize = 50_000_000;

impl SearchIndex {
    pub fn open_or_create(path: &Path) -> Result<Self> {
        std::fs::create_dir_all(path).context("creating tantivy index directory")?;

        let mut schema_builder = Schema::builder();
        let record_id_field = schema_builder.add_text_field("record_id", STRING | STORED);
        let message_field = schema_builder.add_text_field("message", TEXT);
        let schema = schema_builder.build();

        let dir = tantivy::directory::MmapDirectory::open(path)
            .context("opening tantivy mmap directory")?;
        let index =
            Index::open_or_create(dir, schema).context("opening/creating tantivy index")?;

        let writer = index
            .writer(WRITER_HEAP_BYTES)
            .context("creating tantivy index writer")?;

        let reader = index
            .reader_builder()
            .reload_policy(ReloadPolicy::OnCommitWithDelay)
            .try_into()
            .context("building tantivy index reader")?;

        Ok(Self {
            index,
            writer: Mutex::new(writer),
            reader,
            record_id_field,
            message_field,
        })
    }

    /// Upserts one record: delete-then-add on record_id. Tantivy segments
    /// are immutable, so this delete-then-add is the standard idiom for
    /// updates, not a workaround -- and it matters here specifically
    /// because /search's offset tracking is best-effort (see
    /// consumer.rs's OffsetStore), so the same record can genuinely be
    /// reprocessed after an unclean shutdown. Without this, that would
    /// silently duplicate documents instead of just re-writing the same
    /// one.
    pub async fn upsert(&self, record_id: &str, message: &str) -> Result<()> {
        let writer = self.writer.lock().await;
        let term = Term::from_field_text(self.record_id_field, record_id);
        writer.delete_term(term);
        writer
            .add_document(doc!(
                self.record_id_field => record_id,
                self.message_field => message,
            ))
            .context("adding document to tantivy index")?;
        Ok(())
    }

    pub async fn commit(&self) -> Result<()> {
        let mut writer = self.writer.lock().await;
        writer.commit().context("committing tantivy index")?;
        // Explicit reload rather than relying solely on ReloadPolicy::
        // OnCommitWithDelay's background timing: callers of `commit()`
        // (the periodic ticker in consumer.rs, and tests) expect a
        // committed document to be immediately searchable, not visible
        // after some undocumented delay.
        self.reader.reload().context("reloading tantivy reader after commit")?;
        Ok(())
    }

    /// Runs a Tantivy query-parser query against the `message` field,
    /// returning matching record_ids, most-relevant first. Phase 1: no
    /// pagination, no score exposed to the caller — just IDs for `/api`
    /// to join against ClickHouse.
    pub fn search(&self, query: &str, limit: usize) -> Result<Vec<String>> {
        let searcher = self.reader.searcher();
        let query_parser = QueryParser::for_index(&self.index, vec![self.message_field]);
        let parsed_query = query_parser
            .parse_query(query)
            .context("parsing search query")?;
        let top_docs = searcher
            .search(&parsed_query, &TopDocs::with_limit(limit))
            .context("executing search")?;

        let mut ids = Vec::with_capacity(top_docs.len());
        for (_score, doc_address) in top_docs {
            let retrieved: TantivyDocument = searcher
                .doc(doc_address)
                .context("retrieving matched document")?;
            if let Some(value) = retrieved.get_first(self.record_id_field) {
                if let Some(s) = value.as_str() {
                    ids.push(s.to_string());
                }
            }
        }
        Ok(ids)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn new_test_index() -> (SearchIndex, tempfile::TempDir) {
        let dir = tempfile::tempdir().expect("creating temp dir");
        let index = SearchIndex::open_or_create(dir.path()).expect("opening tantivy index");
        (index, dir)
    }

    #[tokio::test]
    async fn upsert_and_search_finds_matching_message() {
        let (index, _dir) = new_test_index();
        index.upsert("id-1", "hello world").await.unwrap();
        index.upsert("id-2", "goodbye moon").await.unwrap();
        index.commit().await.unwrap();

        let results = index.search("hello", 10).unwrap();
        assert_eq!(results, vec!["id-1".to_string()]);
    }

    #[tokio::test]
    async fn search_before_commit_finds_nothing() {
        let (index, _dir) = new_test_index();
        index.upsert("id-1", "hello world").await.unwrap();
        // no commit yet
        let results = index.search("hello", 10).unwrap();
        assert!(results.is_empty(), "expected no results before commit, got {results:?}");
    }

    #[tokio::test]
    async fn upsert_same_id_twice_does_not_duplicate() {
        let (index, _dir) = new_test_index();
        index.upsert("id-1", "hello world").await.unwrap();
        index.commit().await.unwrap();
        index.upsert("id-1", "hello world again").await.unwrap();
        index.commit().await.unwrap();

        let results = index.search("hello", 10).unwrap();
        assert_eq!(
            results.len(),
            1,
            "expected exactly one result after re-upserting the same record_id, got {results:?}"
        );
    }

    #[tokio::test]
    async fn search_respects_limit() {
        let (index, _dir) = new_test_index();
        for i in 0..5 {
            index
                .upsert(&format!("id-{i}"), "shared term")
                .await
                .unwrap();
        }
        index.commit().await.unwrap();

        let results = index.search("shared", 2).unwrap();
        assert_eq!(results.len(), 2);
    }

    #[tokio::test]
    async fn search_supports_phrase_queries() {
        let (index, _dir) = new_test_index();
        index.upsert("id-1", "the quick brown fox").await.unwrap();
        index.upsert("id-2", "quick and brown but not adjacent fox").await.unwrap();
        index.commit().await.unwrap();

        let results = index.search("\"quick brown\"", 10).unwrap();
        assert_eq!(results, vec!["id-1".to_string()]);
    }
}
