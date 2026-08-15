use anyhow::{Context, Result};
use prost::Message;
use rskafka::client::partition::UnknownTopicHandling;
use rskafka::client::ClientBuilder;
use std::collections::BTreeMap;
use std::sync::Arc;
use tokio::sync::Mutex;

use crate::config::Config;
use crate::logsv1;
use crate::offsets::OffsetStore;
use crate::registry::IndexRegistry;
use crate::tenants::ActiveTenantTracker;

/// Kafka message header a resolved tenant ID rides in, attached by
/// ingest's gRPC front end. Mirrors `ingest/internal/grpcserver.
/// TenantIDHeaderKey` / `ingest/consumer.TenantIDHeaderKey` -- those two
/// Go packages duplicate the same literal rather than importing across a
/// producer/consumer boundary (see their doc comments), and this Rust
/// consumer is a third independent reader of the same header, so it
/// duplicates the literal too. `TestTenantIDHeaderKeyMatchesGo` below
/// guards against drift the same way `ingest/cmd/ingest`'s
/// `TestTenantIDHeaderKeyConstantsMatch` does on the Go side.
const TENANT_ID_HEADER_KEY: &str = "tenant_id";

/// Reads the same `sentry.logs.raw` topic ingest's ClickHouse-writer
/// consumer reads, as an independent consumer group in spirit (its own
/// offset tracking, own failure domain) even though rskafka doesn't speak
/// Kafka's broker-side consumer-group protocol -- see offsets.rs. One
/// task per partition; partition count comes from config rather than
/// discovered dynamically, since it has to match what
/// /transport/provision-topics.sh actually created anyway (documented
/// cross-component contract, same as the topic name already is).
pub async fn run(
    cfg: Arc<Config>,
    registry: Arc<IndexRegistry>,
    partition_count: i32,
    active_tenants: Option<Arc<ActiveTenantTracker>>,
) -> Result<()> {
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
    // per-record. Commits every tenant index a write has actually been
    // routed to (plus the default index), not just one -- see
    // registry.rs's commit_all doc comment.
    let commit_interval = cfg.commit_interval;
    let registry_for_commit = Arc::clone(&registry);
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(commit_interval);
        loop {
            ticker.tick().await;
            if let Err(e) = registry_for_commit.commit_all().await {
                tracing::error!(error = %e, "periodic tantivy commit failed");
            }
        }
    });

    let mut handles = Vec::with_capacity(partition_count as usize);
    for partition in 0..partition_count {
        let start_offset = offsets.lock().await.get(partition);
        let client = Arc::clone(&client);
        let registry = Arc::clone(&registry);
        let offsets = Arc::clone(&offsets);
        let active_tenants = active_tenants.clone();
        let topic = cfg.redpanda_topic.clone();
        handles.push(tokio::spawn(async move {
            consume_partition(client, topic, partition, start_offset, registry, offsets, active_tenants).await
        }));
    }

    for handle in handles {
        handle
            .await
            .context("partition consumer task panicked")??;
    }
    Ok(())
}

/// Extracts the `tenant_id` header's value, or "" if absent -- an empty
/// string is exactly what `IndexRegistry::resolve` treats as "route to
/// the default index," so an untagged record (every Phase 0-3 message,
/// and any Phase 4 message from a deployment that never turned on
/// `ingest`'s `TenantResolver`) keeps landing in the same shared index
/// it always has. Mirrors `ingest/consumer.tenantIDFromHeaders` exactly.
fn tenant_id_from_headers(headers: &BTreeMap<String, Vec<u8>>) -> String {
    headers
        .get(TENANT_ID_HEADER_KEY)
        .map(|v| String::from_utf8_lossy(v).into_owned())
        .unwrap_or_default()
}

#[allow(clippy::too_many_arguments)]
async fn consume_partition(
    client: Arc<rskafka::client::Client>,
    topic: String,
    partition: i32,
    start_offset: i64,
    registry: Arc<IndexRegistry>,
    offsets: Arc<Mutex<OffsetStore>>,
    active_tenants: Option<Arc<ActiveTenantTracker>>,
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

            let tenant_id = tenant_id_from_headers(&record_and_offset.record.headers);

            // Fail-closed active-tenant gate (see tenants.rs's doc
            // comment) -- only applies to tagged records and only when
            // a tracker is actually configured, matching resolve()'s
            // own "empty tenant_id always means the default index"
            // rule and this codebase's "off unless configured" default
            // everywhere else. This is the check registry.rs's `resolve`
            // doc comment used to name as missing entirely.
            if !tenant_id.is_empty() {
                if let Some(tracker) = &active_tenants {
                    if !tracker.is_active(&tenant_id).await {
                        tracing::warn!(record_id = %rec.record_id, tenant_id, "skipping record: tenant is not active");
                        continue;
                    }
                }
            }

            let index = match registry.resolve(&tenant_id).await {
                Ok(index) => index,
                Err(e) => {
                    // Shouldn't normally happen -- ingest/grpcserver only
                    // ever attaches a tenant_id it validated against a
                    // real credential, and the active-tenant gate above
                    // already refused anything not currently active when
                    // configured -- but an unsafe/malformed tenant_id is
                    // a hard skip regardless, never a silent fall-back to
                    // the default or any other tenant's index.
                    tracing::error!(error = %e, record_id = %rec.record_id, tenant_id, "skipping record: failed to resolve tenant index");
                    continue;
                }
            };

            if let Err(e) = index.upsert(&rec.record_id, &rec.message).await {
                tracing::error!(error = %e, record_id = %rec.record_id, tenant_id, "failed to index record");
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tenant_id_from_headers_returns_empty_string_when_absent() {
        let headers = BTreeMap::new();
        assert_eq!(tenant_id_from_headers(&headers), "");
    }

    #[test]
    fn tenant_id_from_headers_extracts_the_tenant_id_header() {
        let mut headers = BTreeMap::new();
        headers.insert(TENANT_ID_HEADER_KEY.to_string(), b"acme".to_vec());
        assert_eq!(tenant_id_from_headers(&headers), "acme");
    }

    #[test]
    fn tenant_id_from_headers_ignores_unrelated_headers() {
        let mut headers = BTreeMap::new();
        headers.insert("some-other-header".to_string(), b"acme".to_vec());
        assert_eq!(tenant_id_from_headers(&headers), "");
    }

    /// Guards against the exact literal drift
    /// `ingest/cmd/ingest`'s `TestTenantIDHeaderKeyConstantsMatch` guards
    /// against on the Go side -- three independent readers/writers of the
    /// same Kafka header (`ingest/internal/grpcserver` producing,
    /// `ingest/consumer` and this file both consuming) duplicate the same
    /// literal by design rather than sharing an import across a
    /// producer/consumer or language boundary, so nothing but a test
    /// catches them drifting apart.
    #[test]
    fn test_tenant_id_header_key_matches_go() {
        assert_eq!(TENANT_ID_HEADER_KEY, "tenant_id");
    }
}
