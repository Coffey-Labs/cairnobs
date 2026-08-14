package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// httpGetJSON GETs path and prints the pretty-printed JSON response to
// stdout, or the error body/status to stderr. Shared by dashboards/alerts
// list and get, which otherwise differ only in path and resource name.
func httpGetJSON(baseURL, path string, stdout, stderr io.Writer) int {
	resp, err := httpClient.Get(baseURL + path)
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
func httpPostFileJSON(baseURL, path, file string, stdout, stderr io.Writer) int {
	body, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "reading %s: %v\n", file, err)
		return 1
	}
	resp, err := httpClient.Post(baseURL+path, "application/json", bytes.NewReader(body))
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
