use anyhow::{Context, Result};
use std::collections::HashSet;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::RwLock;

/// How often the tracker re-fetches the active-tenant list after its
/// first successful fetch. Not configurable -- no deployment has needed
/// to tune this yet, and a hardcoded value keeps Config's surface
/// smaller; revisit if that changes.
const REFRESH_INTERVAL: Duration = Duration::from_secs(60);

/// Tracks which tenant_ids are currently `active` in enterprise-auth's
/// `tenants` table, polled from a new `GET /internal/active-tenants`
/// endpoint -- this closes the one gap registry.rs's `resolve` doc
/// comment used to name: `search` (AGPL core) has no Postgres access,
/// so unlike `chwriter.Registry` (an active-tenants-only snapshot built
/// from `rbacstore.ListProvisionedDataSources` at `enterprise-ingest`
/// startup) or the read side (gated by `enterprise/internal/
/// searchclient.TenantChecker`, backed by `rbacstore.TenantIsActive`
/// directly), `consumer.rs`'s write-routing had no allowlist at all --
/// any syntactically-valid `tenant_id` on a still-valid-but-should-
/// have-been-revoked ingest credential could get an index directory
/// created for it.
///
/// Network boundary, not import boundary -- same shape
/// `ingest/internal/grpcserver`'s `TenantResolver` already uses against
/// this exact service, just Rust calling Go instead of Go calling Go,
/// and authenticated the same way `/alerting` authenticates to `/api`:
/// a long-lived RoleService Bearer credential
/// (`enterprise-auth -mint-service-token search`), not a tenant-scoped
/// one -- this tracker proves "I am the search service," never "I may
/// act as tenant X."
///
/// Off unless configured: only constructed when both
/// `ENTERPRISE_AUTH_URL` and `ENTERPRISE_AUTH_SERVICE_TOKEN` are set
/// (see config.rs). When they aren't, `consumer.rs` holds `None` and
/// skips the gate entirely -- every tagged write is routed exactly as
/// it was before this tracker existed, the same "off unless configured"
/// default every other optional integration point in this codebase
/// uses.
pub struct ActiveTenantTracker {
    tenants: RwLock<HashSet<String>>,
}

impl ActiveTenantTracker {
    /// Blocks until the first fetch succeeds. A cold start with
    /// enterprise-auth unreachable must not silently accept every
    /// tenant_id it sees -- that's the exact gap this tracker exists to
    /// close -- so there is deliberately no empty-set-and-keep-going
    /// fallback here; callers should refuse to start the write-routing
    /// consumer at all if this returns an error. Once constructed,
    /// periodic refreshes are best-effort: a transient failure logs and
    /// keeps serving the last-known-good set rather than clearing it
    /// (see the spawned task below) -- only the very first fetch is
    /// fail-closed-to-refusing-startup.
    pub async fn start(base_url: &str, service_token: &str) -> Result<Arc<Self>> {
        let client = reqwest::Client::new();
        let initial = fetch_active_tenants(&client, base_url, service_token)
            .await
            .context("fetching initial active-tenant list from enterprise-auth")?;
        tracing::info!(count = initial.len(), "loaded initial active-tenant list");

        let tracker = Arc::new(Self {
            tenants: RwLock::new(initial),
        });

        let refresh_tracker = Arc::clone(&tracker);
        let base_url = base_url.to_string();
        let service_token = service_token.to_string();
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(REFRESH_INTERVAL);
            ticker.tick().await; // fires immediately -- start() already fetched once, skip it
            loop {
                ticker.tick().await;
                match fetch_active_tenants(&client, &base_url, &service_token).await {
                    Ok(fresh) => {
                        let count = fresh.len();
                        *refresh_tracker.tenants.write().await = fresh;
                        tracing::debug!(count, "refreshed active-tenant list");
                    }
                    Err(e) => {
                        // No staleness ceiling: a prolonged enterprise-auth
                        // outage means the allowlist just doesn't grow or
                        // shrink until connectivity resumes, disclosed here
                        // rather than degrading further (e.g. clearing the
                        // set, which would stop every tenant's indexing on
                        // one control-plane blip -- a worse blast radius
                        // than staleness).
                        tracing::error!(error = %e, "failed to refresh active-tenant list, keeping last-known-good set");
                    }
                }
            }
        });

        Ok(tracker)
    }

    pub async fn is_active(&self, tenant_id: &str) -> bool {
        self.tenants.read().await.contains(tenant_id)
    }
}

#[derive(serde::Deserialize)]
struct ActiveTenantsResponse {
    tenant_ids: Vec<String>,
}

async fn fetch_active_tenants(
    client: &reqwest::Client,
    base_url: &str,
    service_token: &str,
) -> Result<HashSet<String>> {
    let resp = client
        .get(format!("{base_url}/internal/active-tenants"))
        .bearer_auth(service_token)
        .send()
        .await
        .context("sending request")?
        .error_for_status()
        .context("non-2xx response")?
        .json::<ActiveTenantsResponse>()
        .await
        .context("parsing response body")?;
    Ok(resp.tenant_ids.into_iter().collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    /// Minimal hand-rolled HTTP/1.1 server -- one dependency-free helper
    /// rather than pulling in a mocking crate for the one endpoint this
    /// module ever calls. Reads one request, hands it (as raw bytes) to
    /// `respond`, writes back exactly what `respond` returns, then
    /// closes -- enough to exercise real reqwest request construction
    /// (the Bearer header, the URL path) and real response parsing, not
    /// a fake client substituted in.
    async fn spawn_fake_server(
        respond: impl Fn(&str) -> String + Send + Sync + 'static,
    ) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let (mut stream, _) = match listener.accept().await {
                    Ok(v) => v,
                    Err(_) => return,
                };
                let mut buf = vec![0u8; 8192];
                let n = stream.read(&mut buf).await.unwrap_or(0);
                let request = String::from_utf8_lossy(&buf[..n]).to_string();
                let response = respond(&request);
                let _ = stream.write_all(response.as_bytes()).await;
            }
        });
        format!("http://{addr}")
    }

    fn json_response(status_line: &str, body: &str) -> String {
        format!(
            "{status_line}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    #[tokio::test]
    async fn start_fetches_and_serves_the_initial_list() {
        let base_url = spawn_fake_server(|_req| {
            json_response("HTTP/1.1 200 OK", r#"{"tenant_ids":["acme","globex"]}"#)
        })
        .await;

        let tracker = ActiveTenantTracker::start(&base_url, "test-token")
            .await
            .expect("start should succeed against a healthy fake server");

        assert!(tracker.is_active("acme").await);
        assert!(tracker.is_active("globex").await);
        assert!(!tracker.is_active("initech").await);
    }

    #[tokio::test]
    async fn start_sends_the_bearer_token() {
        let base_url = spawn_fake_server(|req| {
            if req.contains("authorization: Bearer secret-token") {
                json_response("HTTP/1.1 200 OK", r#"{"tenant_ids":["acme"]}"#)
            } else {
                json_response("HTTP/1.1 401 Unauthorized", r#"{"error":"no credentials presented"}"#)
            }
        })
        .await;

        let tracker = ActiveTenantTracker::start(&base_url, "secret-token")
            .await
            .expect("start should succeed once the fake server sees the right token");
        assert!(tracker.is_active("acme").await);
    }

    #[tokio::test]
    async fn start_fails_closed_when_the_first_fetch_fails() {
        let base_url = spawn_fake_server(|_req| {
            json_response("HTTP/1.1 401 Unauthorized", r#"{"error":"invalid or expired credentials"}"#)
        })
        .await;

        let result = ActiveTenantTracker::start(&base_url, "wrong-token").await;
        assert!(
            result.is_err(),
            "expected start() to fail (not silently start with an empty/permissive allowlist) when the first fetch fails"
        );
    }

    #[tokio::test]
    async fn start_fails_closed_when_the_server_is_unreachable() {
        // Port 1 is (almost certainly) not listening -- connection refused,
        // not a slow timeout, so this test stays fast.
        let result = ActiveTenantTracker::start("http://127.0.0.1:1", "any-token").await;
        assert!(result.is_err());
    }
}
