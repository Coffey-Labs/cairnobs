use crate::config::{IngestConfig, TlsConfig};
use crate::pb::agent::v1::{agent_control_client::AgentControlClient, CheckInRequest, CheckInResponse};
use crate::pb::{log_ingest_client::LogIngestClient, LogRecord, PushBatchRequest};
use anyhow::{Context, Result};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};

/// Establishes the one mTLS gRPC channel an agent has to ingest —
/// agents never talk to Redpanda directly, and never accept an inbound
/// connection either (see /docs/architecture.md and
/// /docs/agent-management-design.md). Returns the bare `Channel` rather
/// than a client wrapper so callers can build both `LogIngestClient`
/// (data plane) and `AgentControlClient` (control plane) from the same
/// connection — `Channel` is a cheap-to-clone handle, not the socket
/// itself, so there's no cost to sharing it across two client stubs.
pub async fn connect(ingest: &IngestConfig, tls: &TlsConfig) -> Result<Channel> {
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

    Channel::from_shared(ingest.endpoint.clone())
        .context("invalid ingest endpoint URL")?
        .tls_config(tls_config)
        .context("configuring mTLS")?
        .connect()
        .await
        .context("connecting to ingest service")
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

pub async fn check_in(
    client: &mut AgentControlClient<Channel>,
    req: CheckInRequest,
) -> Result<CheckInResponse> {
    let resp = client.check_in(req).await.context("CheckIn RPC failed")?;
    Ok(resp.into_inner())
}
