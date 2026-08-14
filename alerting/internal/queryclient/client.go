// Package queryclient is a thin HTTP client to /api's POST /query --
// alerting never imports querylang or talks to ClickHouse/Tantivy
// directly, same precedent sentryctl query and the web UI's dashboard
// panels already set: one query-execution path, reused everywhere.
package queryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Result struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{}}
}

// Query runs query (already time-range-injected by the caller, if
// applicable) against /api's POST /query and returns the result. A
// non-2xx response or a request/decode failure is returned as an error
// -- callers (the evaluator) treat any error here as an evaluation
// error, never as "condition false" (see
// /docs/phase-3-alerting-design.md's fix 3).
func (c *Client) Query(ctx context.Context, query, language string, timeout time.Duration) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"query": query, "language": language})
	if err != nil {
		return nil, fmt.Errorf("encoding query request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling api /query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody errorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			return nil, fmt.Errorf("api /query failed (%d): %s", resp.StatusCode, errBody.Error)
		}
		return nil, fmt.Errorf("api /query failed with status %d", resp.StatusCode)
	}

	var result Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding query response: %w", err)
	}
	return &result, nil
}
