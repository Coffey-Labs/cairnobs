// Package tenantresolver is ingest's HTTP client for resolving an
// agent-presented ingest credential to a tenant -- calls enterprise-
// auth's POST /internal/authorize-ingest over the network, never
// importing enterprise/ (both ingest and enterprise/ are AGPLv3 as of
// Phase 6; the import boundary is architectural, not a licensing wall --
// same "network boundary, not import boundary" shape api/authz.HTTPAuthorizer
// already uses for the query path, and enterprise-auth's own doc
// comment on POST /internal/authorize-ingest). nil (no resolver
// configured) is grpcserver.Server's documented no-op default --
// single-tenant deployments never construct one, and every record's
// TenantID stays empty, exactly like before per-tenant ingest
// credentials existed.
package tenantresolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type HTTPResolver struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *HTTPResolver {
	return &HTTPResolver{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

type authorizeIngestResponse struct {
	TenantID string `json:"tenant_id"`
}

// ResolveTenant implements grpcserver.TenantResolver. Forwards only the
// bearer token itself, nothing else about the caller's request -- same
// "forward exactly the credential, never the rest of the request"
// discipline api/authz.HTTPAuthorizer already follows.
func (r *HTTPResolver) ResolveTenant(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/internal/authorize-ingest", nil)
	if err != nil {
		return "", fmt.Errorf("tenantresolver: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tenantresolver: calling enterprise-auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tenantresolver: enterprise-auth returned status %d", resp.StatusCode)
	}

	var body authorizeIngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("tenantresolver: decoding response: %w", err)
	}
	if body.TenantID == "" {
		return "", fmt.Errorf("tenantresolver: enterprise-auth returned an empty tenant_id")
	}
	return body.TenantID, nil
}
