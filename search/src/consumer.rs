use anyhow::{Context, Result};
use prost::Message;
use rskafka::client::partition::UnknownTopicHandling;
use rskafka::client::ClientBuilder;
use std::sync::Arc;
use tokio::sync::Mutex;

use crate::config::Config;
use crate::index::SearchIndex;
use crate::logsv1;
use crate::offsets::OffsetStore;

/// Reads the same `sentry.logs.raw` topic ingest's ClickHouse-writer
/// consumer reads, as an independent consumer group in spirit (its own
/// offset tracking, own failure domain) even though rskafka doesn't speak
/// Kafka's broker-side consumer-group protocol -- see offsets.rs. One
/// task per partition; partition count comes from config rather than
/// discovered dynamically, since it has to match what
/// /transport/provision-topics.sh actually created anyway (documented
/// cross-component contract, same as the topic name already is).
pub async fn run(cfg: Arc<Config>, index: Arc<SearchIndex>, partition_count: i32) -> Result<()> {
    let client = ClientBuilder::new(cfg.redpanda_brokers.clone())
        .build()
        .await
        .context("building rskafka client")?;
    let client = Arc::new(client);

    let offsets = OffsetStore::load(&cfg.offsets_path)
        .await
        .context("loading offset store")?;
    let offsets = Arc::new(Mutex::new(offsets));

    // Periodic Tantivy commit, batched for throughput the same way
    // ingest's ClickHouse writer batches inserts rather than inserting
    // per-record.
    let commit_interval = cfg.commit_interval;
    let index_for_commit = Arc::clone(&index);
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(commit_interval);
        loop {
            ticker.tick().await;
            if let Err(e) = index_for_commit.commit().await {
                tracing::error!(error = %e, "periodic tantivy commit failed");
            }
        }
    });

    let mut handles = Vec::with_capacity(partition_count as usize);
    for partition in 0..partition_count {
        let start_offset = offsets.lock().await.get(partition);
        let client = Arc::clone(&client);
        let index = Arc::clone(&index);
        let offsets = Arc::clone(&offsets);
        let topic = cfg.redpanda_topic.clone();
        handles.push(tokio::spawn(async move {
            consume_partition(client, topic, partition, start_offset, index, offsets).await
        }));
    }

    for handle in handles {
        handle
            .await
            .context("partition consumer task panicked")??;
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn consume_partition(
    client: Arc<rskafka::client::Client>,
    topic: String,
    partition: i32,
    start_offset: i64,
    index: Arc<SearchIndex>,
    offsets: Arc<Mutex<OffsetStore>>,
) -> Result<()> {
    let partition_client = client
        .partition_client(topic.clone(), partition, UnknownTopicHandling::Error)
        .await
        .with_context(|| format!("creating partition client for {topic}[{partition}]"))?;

    let mut offset = start_offset;
    loop {
        let (records, _high_watermark) = partition_client
            .fetch_records(offset, 1..1_000_000, 5_000)
            .await
            .with_context(|| {
                format!("fetching records from {topic}[{partition}] at offset {offset}")
            })?;

        if records.is_empty() {
            continue;
        }

        for record_and_offset in &records {
            offset = record_and_offset.offset + 1;

            let Some(value) = &record_and_offset.record.value else {
                continue;
            };
            let rec = match logsv1::LogRecord::decode(value.as_slice()) {
                Ok(rec) => rec,
                Err(e) => {
                    tracing::warn!(error = %e, partition, "skipping unparseable message");
                    continue;
                }
            };

            if rec.record_id.is_empty() {
                // Shouldn't happen -- ingest's gRPC front end always
                // assigns this before producing -- but a message with no
                // ID can't be joined back to a ClickHouse row, so it's
                // useless to index.
                tracing::warn!(partition, "skipping record with empty record_id");
                continue;
            }

            if let Err(e) = index.upsert(&rec.record_id, &rec.message).await {
                tracing::error!(error = %e, record_id = %rec.record_id, "failed to index record");
            }
        }

        // Persisted after each fetched batch, not per-record: worst-case
        // reprocessing on an unclean restart is one batch, which
        // `SearchIndex::upsert`'s delete-then-add makes harmless anyway.
        {
            let mut offsets = offsets.lock().await;
            offsets.set(partition, offset);
            if let Err(e) = offsets.persist().await {
                tracing::error!(error = %e, partition, "failed to persist offset");
            }
        }
    }
}
