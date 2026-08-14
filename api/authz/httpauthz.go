package authz

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPAuthorizer is the production Authorizer for a deployment with
// enterprise-auth configured -- it calls enterprise-auth's
// POST /internal/authorize endpoint, forwarding the caller's own
// credentials (session cookie or service-token header), rather than
// importing any enterprise/ Go package. This is the same "network
// boundary, not import boundary" shape /alerting's queryclient already
// uses to call api's /query: the module-boundary guarantee
// hack/check-tenant-boundary.sh enforces is about Go imports, and this
// type has none from enterprise/.
type HTTPAuthorizer struct {
	baseURL string
	http    *http.Client
}

func NewHTTPAuthorizer(baseURL string) *HTTPAuthorizer {
	return &HTTPAuthorizer{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

type authorizeResponse struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
}

// Authorize forwards exactly two credential-carrying headers -- Cookie
// (human sessions) and Authorization (service tokens) -- never the rest
// of the request. enterprise-auth validates whichever is present and
// returns the resolved identity, or a non-2xx if neither validates.
func (a *HTTPAuthorizer) Authorize(r *http.Request) (Identity, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.baseURL+"/internal/authorize", nil)
	if err != nil {
		return Identity{}, fmt.Errorf("authz: building request: %w", err)
	}
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("authz: calling enterprise-auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("authz: enterprise-auth returned status %d", resp.StatusCode)
	}

	var body authorizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Identity{}, fmt.Errorf("authz: decoding response: %w", err)
	}
	return Identity{TenantID: body.TenantID, UserID: body.UserID, Role: Role(body.Role)}, nil
}
