use crate::config::{IngestConfig, TlsConfig};
use crate::pb::{log_ingest_client::LogIngestClient, LogRecord, PushBatchRequest};
use anyhow::{Context, Result};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};

/// Establishes an mTLS gRPC channel to the ingest service. Agents never
/// talk to Redpanda directly — this is the only network egress the agent
/// has, by design (see /docs/architecture.md).
pub async fn connect(ingest: &IngestConfig, tls: &TlsConfig) -> Result<LogIngestClient<Channel>> {
    let ca = tokio::fs::read(&tls.ca_cert)
        .await
        .with_context(|| format!("reading CA cert at {}", tls.ca_cert.display()))?;
    let cert = tokio::fs::read(&tls.client_cert)
        .await
        .with_context(|| format!("reading client cert at {}", tls.client_cert.display()))?;
    let key = tokio::fs::read(&tls.client_key)
        .await
        .with_context(|| format!("reading client key at {}", tls.client_key.display()))?;

    let tls_config = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(ca))
        .identity(Identity::from_pem(cert, key));

    let channel = Channel::from_shared(ingest.endpoint.clone())
        .context("invalid ingest endpoint URL")?
        .tls_config(tls_config)
        .context("configuring mTLS")?
        .connect()
        .await
        .context("connecting to ingest service")?;

    Ok(LogIngestClient::new(channel))
}

pub async fn send_batch(
    client: &mut LogIngestClient<Channel>,
    batch_id: String,
    records: Vec<LogRecord>,
) -> Result<u32> {
    let resp = client
        .push_batch(PushBatchRequest { batch_id, records })
        .await
        .context("PushBatch RPC failed")?;
    Ok(resp.into_inner().accepted)
}
