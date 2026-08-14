use std::sync::Arc;
use tonic::{Request, Response, Status};

use crate::registry::IndexRegistry;
use crate::searchv1;

const DEFAULT_LIMIT: usize = 100;

pub struct SearchServer {
    registry: Arc<IndexRegistry>,
}

impl SearchServer {
    pub fn new(registry: Arc<IndexRegistry>) -> Self {
        Self { registry }
    }
}

#[tonic::async_trait]
impl searchv1::search_service_server::SearchService for SearchServer {
    async fn search(
        &self,
        request: Request<searchv1::SearchRequest>,
    ) -> Result<Response<searchv1::SearchResponse>, Status> {
        let req = request.into_inner();
        if req.query.trim().is_empty() {
            return Err(Status::invalid_argument("query must not be empty"));
        }

        let limit = if req.limit == 0 {
            DEFAULT_LIMIT
        } else {
            req.limit as usize
        };

        // Resolves (opening on first use) the caller's tenant index, or
        // the single default index when tenant_id is empty -- see
        // registry.rs's doc comment. Never falls back to a *different*
        // tenant's index on error; an unsafe/unknown tenant_id is a
        // hard failure, not a silent default.
        let index = self
            .registry
            .resolve(&req.tenant_id)
            .await
            .map_err(|e| Status::invalid_argument(format!("resolving tenant index: {e}")))?;

        let query = req.query.clone();
        // Tantivy's searcher is synchronous; run it on a blocking thread
        // so it doesn't stall the async runtime alongside the consumer
        // tasks.
        let record_ids = tokio::task::spawn_blocking(move || index.search(&query, limit))
            .await
            .map_err(|e| Status::internal(format!("search task panicked: {e}")))?
            .map_err(|e| Status::invalid_argument(format!("search failed: {e}")))?;

        Ok(Response::new(searchv1::SearchResponse { record_ids }))
    }
}
