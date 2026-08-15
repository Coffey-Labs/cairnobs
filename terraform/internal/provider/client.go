package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// client is a thin HTTP client against api/dashboards.Handler's REST
// endpoints -- deliberately hand-rolled, not generated from an OpenAPI
// spec (none exists in this repo yet), the same "boring, well-
// understood" posture cli/cmd/sentryctl's own httpclient.go already
// takes against the same API. Kept separate from that package (not
// reused directly) since this one needs typed request/response
// marshaling for Terraform's plan/state model, where sentryctl only
// ever needs to pretty-print whatever JSON comes back.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{baseURL: baseURL, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// apiError carries the HTTP status code through so callers can
// distinguish "the server rejected this request" from "this specific
// resource doesn't exist" (isNotFound below) -- Read/Delete need that
// distinction to implement Terraform's standard "drop from state, don't
// error the whole apply" convention for a resource deleted out-of-band.
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("sentry api: request failed with status %d: %s", e.StatusCode, e.Message)
}

func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// dashboard mirrors api/dashboards.Dashboard's JSON shape -- deliberately
// a local type, not an import of that package (this module has no
// dependency on /api at all, matching every other cross-module boundary
// in this repo: talk over HTTP, not Go imports, to a service that isn't
// yours). Panels are intentionally not modeled here yet -- this
// resource only manages dashboard-level fields; see the provider
// README for why panels are scoped-out future work, not an oversight.
type dashboard struct {
	ID              string `json:"id,omitempty"`
	TenantID        string `json:"tenant_id,omitempty"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	DefaultEarliest string `json:"default_earliest,omitempty"`
	DefaultLatest   string `json:"default_latest,omitempty"`
	CreatedBy       string `json:"created_by,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := string(respBody)
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return &apiError{StatusCode: resp.StatusCode, Message: msg}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *client) createDashboard(ctx context.Context, d *dashboard) (*dashboard, error) {
	var out dashboard
	if err := c.do(ctx, http.MethodPost, "/dashboards", d, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) getDashboard(ctx context.Context, id string) (*dashboard, error) {
	var out dashboard
	if err := c.do(ctx, http.MethodGet, "/dashboards/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) updateDashboard(ctx context.Context, id string, d *dashboard) (*dashboard, error) {
	var out dashboard
	if err := c.do(ctx, http.MethodPut, "/dashboards/"+id, d, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) deleteDashboard(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/dashboards/"+id, nil, nil)
}

// rule mirrors alerting/internal/rulestore.Rule's JSON shape, plus the
// request-only `enabled` field POST /rules accepts
// (httpapi.createRuleRequest embeds rulestore.Rule and adds this
// pointer specifically so "omitted" (defaults to enabled) and
// "explicitly false" are distinguishable -- see handleCreateRule's doc
// comment) -- deliberately a local type, not an import of either
// package, same "talk HTTP, not Go imports, to a service that isn't
// yours" posture as dashboard above. GET/POST /rules both return this
// shape flattened (no separate "state" wrapper needed here since this
// resource doesn't manage or expose alert_state -- see the provider
// README on why).
type rule struct {
	ID                      string   `json:"id,omitempty"`
	TenantID                string   `json:"tenant_id,omitempty"`
	Name                    string   `json:"name"`
	Description             string   `json:"description"`
	Query                   string   `json:"query"`
	QueryLanguage           string   `json:"query_language"`
	ConditionType           string   `json:"condition_type"`
	Comparator              *string  `json:"comparator,omitempty"`
	ThresholdValue          *float64 `json:"threshold_value,omitempty"`
	EvalIntervalSeconds     int      `json:"eval_interval_seconds"`
	ForMinutes              int      `json:"for_minutes"`
	RenotifyIntervalMinutes *int     `json:"renotify_interval_minutes,omitempty"`
	NotificationTargetID    string   `json:"notification_target_id"`
	Enabled                 *bool    `json:"enabled,omitempty"`
	CreatedBy               string   `json:"created_by,omitempty"`
}

// createRule and getRule are the only mutating/reading calls this
// client makes against /rules -- there is deliberately no updateRule:
// alerting/internal/httpapi has no PUT /rules/{id} at all (confirmed
// down to rulestore.Store, which has Create/List/Get/Delete but no
// Update method to even wire one to) -- a real, pre-existing gap in
// alerting's own API, not something this provider works around by
// faking an update via delete+recreate under the hood. alertRuleResource
// models this honestly: every attribute is RequiresReplace, so
// Terraform destroys and recreates on any change rather than pretending
// an in-place update exists.
func (c *client) createRule(ctx context.Context, r *rule) (*rule, error) {
	var out rule
	if err := c.do(ctx, http.MethodPost, "/rules", r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) getRule(ctx context.Context, id string) (*rule, error) {
	var out rule
	if err := c.do(ctx, http.MethodGet, "/rules/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) deleteRule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/rules/"+id, nil, nil)
}
