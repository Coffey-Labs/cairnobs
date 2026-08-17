package main

import (
	"bufio"
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
	// nlQuery, execute (Phase 7 task 11): --nl routes through
	// POST /ai/translate instead of running query text directly.
	// execute is the same "explicit opt-in to actually run this"
	// posture the web UI's confirm-to-run action enforces -- a
	// translated query never runs itself, here or there.
	nlQuery string
	execute bool
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
		case "--nl":
			if i+1 < len(args) {
				qa.nlQuery = args[i+1]
				i++
			}
		case "--execute":
			qa.execute = true
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
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	Warnings []string `json:"warnings"`
}

type translateRequestBody struct {
	NLQuery string `json:"nlQuery"`
}

type translateResponseBody struct {
	Query               string   `json:"query"`
	Confidence          string   `json:"confidence"`
	LowConfidenceReason string   `json:"lowConfidenceReason"`
	Compiles            bool     `json:"compiles"`
	CompileError        string   `json:"compileError"`
	Blocked             bool     `json:"blocked"`
	CostWarnings        []string `json:"costWarnings"`
}

func cmdQuery(args []string, stdout, stderr io.Writer) int {
	qa := parseQueryArgs(args, os.Getenv)

	if qa.nlQuery != "" {
		return cmdQueryNL(qa, stdout, stderr, os.Stdin)
	}

	if strings.TrimSpace(qa.query) == "" {
		fmt.Fprintln(stderr, "sentryctl query: missing query string")
		return 1
	}
	return runAndPrintQuery(qa.apiURL, qa.query, qa.language, qa.jsonOut, stdout, stderr)
}

// cmdQueryNL implements --nl: translate, show the result, then only run
// it with explicit opt-in (--execute, or an interactive "y" confirmation
// -- never a bare unattended run). stdin is a parameter (not read from
// os.Stdin directly) so the confirmation prompt is testable the same
// way parseQueryArgs's env injection is.
func cmdQueryNL(qa queryArgs, stdout, stderr io.Writer, stdin io.Reader) int {
	reqBody, err := json.Marshal(translateRequestBody{NLQuery: qa.nlQuery})
	if err != nil {
		fmt.Fprintf(stderr, "encoding request: %v\n", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, qa.apiURL+"/ai/translate", bytes.NewReader(reqBody))
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, resolveToken(os.Getenv))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "translation failed: %v\n", err)
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
			fmt.Fprintf(stderr, "translation failed: %s\n", errResp.Error)
		} else {
			fmt.Fprintf(stderr, "translation failed: api returned status %d\n", resp.StatusCode)
		}
		return 1
	}

	var t translateResponseBody
	if err := json.Unmarshal(respBody, &t); err != nil {
		fmt.Fprintf(stderr, "decoding response: %v\n", err)
		return 1
	}

	if t.Query == "" {
		reason := t.LowConfidenceReason
		if reason == "" {
			reason = "the model did not return a query"
		}
		fmt.Fprintf(stdout, "No confident translation available: %s\n", reason)
		return 1
	}

	fmt.Fprintf(stdout, "Translated query (%s confidence):\n  %s\n", t.Confidence, t.Query)
	if !t.Compiles {
		fmt.Fprintf(stdout, "This does not parse as a valid query: %s\n", t.CompileError)
		fmt.Fprintln(stdout, "Not running it -- copy, fix, and run manually if you want to use it.")
		return 1
	}
	if len(t.CostWarnings) > 0 {
		fmt.Fprintf(stdout, "Cost guard: %s\n", strings.Join(t.CostWarnings, "; "))
	}
	if t.Blocked {
		fmt.Fprintln(stdout, "Not offered as directly runnable -- copy and adjust manually if you want to use it.")
		return 1
	}

	if !qa.execute {
		if !isInteractive(stdin) {
			fmt.Fprintln(stdout, "Not running (pass --execute to run automatically, or run this interactively to confirm).")
			return 0
		}
		if !confirmRun(stdin, stdout, "Run this query?") {
			fmt.Fprintln(stdout, "Not running.")
			return 0
		}
	}

	return runAndPrintQuery(qa.apiURL, t.Query, "spl", qa.jsonOut, stdout, stderr)
}

// confirmRun prompts stdin for a y/N answer -- only "y"/"yes"
// (case-insensitive) counts as confirmation, matching the web UI's
// posture that running an AI-generated query is opt-in, never a
// default a blank Enter press could accidentally trigger.
func confirmRun(stdin io.Reader, stdout io.Writer, prompt string) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// isInteractive reports whether stdin looks like a real terminal --
// used to decide whether a confirmation prompt makes sense at all
// (a non-interactive/piped invocation with no --execute would otherwise
// hang forever waiting for an answer nobody can give; refusing to run
// and exiting cleanly is the safe default there, matching --execute's
// own opt-in-required posture rather than silently running).
func isInteractive(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func runAndPrintQuery(apiURL, query, language string, jsonOut bool, stdout, stderr io.Writer) int {
	reqBody, err := json.Marshal(queryRequestBody{Query: query, Language: language})
	if err != nil {
		fmt.Fprintf(stderr, "encoding request: %v\n", err)
		return 1
	}

	req, err := http.NewRequest(http.MethodPost, apiURL+"/query", bytes.NewReader(reqBody))
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

	if jsonOut {
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
	if len(result.Warnings) > 0 {
		fmt.Fprintf(stdout, "\nWarning: %s\n", strings.Join(result.Warnings, "; "))
	}
	return 0
}
