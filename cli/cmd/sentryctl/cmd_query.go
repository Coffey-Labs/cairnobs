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

type queryArgs struct {
	apiURL   string
	jsonOut  bool
	language string
	query    string
}

// parseQueryArgs is pure (env passed in, no I/O), same testability
// reasoning as parsePingArgs. Non-flag arguments are joined with spaces
// to form the query, so `sentryctl query service=api status=500` (no
// quotes, no shell-special characters) works without requiring users to
// quote every query -- though anything using "|" still needs shell
// quoting regardless, since that's a real shell pipe character otherwise.
func parseQueryArgs(args []string, env func(string) string) queryArgs {
	qa := queryArgs{apiURL: resolveAPIURL(env)}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api":
			if i+1 < len(args) {
				qa.apiURL = args[i+1]
				i++
			}
		case "--json":
			qa.jsonOut = true
		case "--language":
			if i+1 < len(args) {
				qa.language = args[i+1]
				i++
			}
		default:
			rest = append(rest, args[i])
		}
	}
	qa.query = strings.Join(rest, " ")
	return qa
}

type queryRequestBody struct {
	Query    string `json:"query"`
	Language string `json:"language"`
}

type queryResponseBody struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func cmdQuery(args []string, stdout, stderr io.Writer) int {
	qa := parseQueryArgs(args, os.Getenv)
	if strings.TrimSpace(qa.query) == "" {
		fmt.Fprintln(stderr, "sentryctl query: missing query string")
		return 1
	}

	reqBody, err := json.Marshal(queryRequestBody{Query: qa.query, Language: qa.language})
	if err != nil {
		fmt.Fprintf(stderr, "encoding request: %v\n", err)
		return 1
	}

	req, err := http.NewRequest(http.MethodPost, qa.apiURL+"/query", bytes.NewReader(reqBody))
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, resolveToken(os.Getenv))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "query failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "reading response: %v\n", err)
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		var errResp errorResponseBody
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			fmt.Fprintf(stderr, "query failed: %s\n", errResp.Error)
		} else {
			fmt.Fprintf(stderr, "query failed: api returned status %d\n", resp.StatusCode)
		}
		return 1
	}

	if qa.jsonOut {
		_, _ = stdout.Write(respBody)
		fmt.Fprintln(stdout)
		return 0
	}

	var result queryResponseBody
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Fprintf(stderr, "decoding response: %v\n", err)
		return 1
	}
	printTable(stdout, result.Columns, result.Rows)
	return 0
}
