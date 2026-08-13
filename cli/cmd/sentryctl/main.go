// Command sentryctl is Sentry's control CLI. Phase 0: a single "ping"
// command that checks the api service is reachable. More commands land as
// the control plane grows real operations to expose.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
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
	fmt.Fprintln(w, `sentryctl: Sentry control CLI (Phase 0: ping only)

Usage:
  sentryctl ping [--api <url>]

Commands:
  ping   Checks that the api service is reachable via GET /healthz.

--api defaults to $SENTRYCTL_API_URL, or `+defaultAPIURL+` if unset.`)
}

// parsePingArgs resolves the api base URL for ping: --api flag wins, then
// $SENTRYCTL_API_URL, then the hardcoded default. Kept pure (env passed in
// as a function) and separate from the HTTP call so it's unit-testable
// without a real environment or server.
func parsePingArgs(args []string, env func(string) string) string {
	apiURL := env("SENTRYCTL_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
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
