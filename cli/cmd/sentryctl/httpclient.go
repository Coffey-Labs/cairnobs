package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// setAuth attaches SENTRYCTL_TOKEN (see resolveToken) as a Bearer
// credential, a no-op when token is empty -- matches every backend's
// nil-authorizer no-op default (see api/internal/authz.RequireRole*).
func setAuth(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// httpGetJSON GETs path and prints the pretty-printed JSON response to
// stdout, or the error body/status to stderr. Shared by dashboards/alerts
// list and get, which otherwise differ only in path and resource name.
func httpGetJSON(baseURL, path, token string, stdout, stderr io.Writer) int {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	setAuth(req, token)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	return printJSONResponse(resp, stdout, stderr)
}

// httpPostFileJSON reads file (a JSON document, e.g. an exported
// dashboard or a rule definition) and POSTs it to path as-is -- no
// reshaping, since the file's shape already matches what the endpoint
// expects (the same JSON the web UI's export button and POST /rules
// produce/accept respectively). This is what makes "apply" the seed of a
// future Terraform provider: one JSON contract, multiple callers.
func httpPostFileJSON(baseURL, path, token, file string, stdout, stderr io.Writer) int {
	body, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "reading %s: %v\n", file, err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, token)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	return printJSONResponse(resp, stdout, stderr)
}

// httpMutateNoBody sends method to path with an optional JSON body
// ("" for none, e.g. DELETE) and expects a 2xx with no meaningful
// response body -- PUT/DELETE /dashboards/{id}/permissions/{userId}
// both respond 204 No Content, so there's nothing for printJSONResponse
// to pretty-print here. Prints successMsg to stdout on success, the
// same {"error": "..."} parsing every other helper in this file uses
// otherwise.
func httpMutateNoBody(method, baseURL, path, token, body, successMsg string, stdout, stderr io.Writer) int {
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reqBody)
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	setAuth(req, token)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		var errResp errorResponseBody
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			fmt.Fprintf(stderr, "request failed: %s\n", errResp.Error)
		} else {
			fmt.Fprintf(stderr, "request failed: status %d\n", resp.StatusCode)
		}
		return 1
	}
	fmt.Fprintln(stdout, successMsg)
	return 0
}

// httpPutJSON PUTs body (already-encoded JSON) to path and prints the
// pretty-printed JSON response -- same shape as httpPostFileJSON, but
// for callers that construct the body themselves rather than reading it
// from a file (agents config set, agents restart).
func httpPutJSON(baseURL, path, token, body string, stdout, stderr io.Writer) int {
	req, err := http.NewRequest(http.MethodPut, baseURL+path, strings.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, token)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	return printJSONResponse(resp, stdout, stderr)
}

func printJSONResponse(resp *http.Response, stdout, stderr io.Writer) int {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "reading response: %v\n", err)
		return 1
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp errorResponseBody
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			fmt.Fprintf(stderr, "request failed: %s\n", errResp.Error)
		} else {
			fmt.Fprintf(stderr, "request failed: status %d\n", resp.StatusCode)
		}
		return 1
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		stdout.Write(pretty.Bytes())
	} else {
		stdout.Write(body)
	}
	fmt.Fprintln(stdout)
	return 0
}
