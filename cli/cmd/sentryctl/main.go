// Command sentryctl is Sentry's control CLI: "ping" (Phase 0) and
// "query" (Phase 2), which accepts either query syntax and hits the same
// POST /query endpoint the web UI does -- no separate query logic here,
// per the Phase 2 task list's explicit instruction.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const defaultAPIURL = "http://localhost:8080"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 1
	}

	switch args[0] {
	case "ping":
		return cmdPing(args[1:], stdout, stderr)
	case "query":
		return cmdQuery(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "sentryctl: unknown command %q\n", args[0])
		usage(stderr)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `sentryctl: Sentry control CLI

Usage:
  sentryctl ping [--api <url>]
  sentryctl query "<query>" [--api <url>] [--language sql|spl] [--json]

Commands:
  ping    Checks that the api service is reachable via GET /healthz.
  query   Runs a query (pipe syntax or SQL) against POST /query and
          prints the result as a table, or as JSON with --json. Quote
          the query in your shell -- pipe syntax uses "|", which your
          shell will otherwise interpret itself.

--api defaults to $SENTRYCTL_API_URL, or `+defaultAPIURL+` if unset.
--language overrides auto-detection; omit it for the common case.`)
}

func resolveAPIURL(env func(string) string) string {
	if v := env("SENTRYCTL_API_URL"); v != "" {
		return v
	}
	return defaultAPIURL
}

// parsePingArgs resolves the api base URL for ping: --api flag wins, then
// $SENTRYCTL_API_URL, then the hardcoded default. Kept pure (env passed in
// as a function) and separate from the HTTP call so it's unit-testable
// without a real environment or server.
func parsePingArgs(args []string, env func(string) string) string {
	apiURL := resolveAPIURL(env)
	for i := 0; i < len(args); i++ {
		if args[i] == "--api" && i+1 < len(args) {
			apiURL = args[i+1]
			i++
		}
	}
	return apiURL
}

func cmdPing(args []string, stdout, stderr io.Writer) int {
	apiURL := parsePingArgs(args, os.Getenv)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "ping failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "ping failed: api returned status %d\n", resp.StatusCode)
		return 1
	}

	fmt.Fprintln(stdout, "ok")
	return 0
}

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

type errorResponseBody struct {
	Error string `json:"error"`
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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(qa.apiURL+"/query", "application/json", bytes.NewReader(reqBody))
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

func printTable(w io.Writer, columns []string, rows [][]any) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(columns, "\t"))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = formatCell(v)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "(%d row(s))\n", len(rows))
}

func formatCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}
