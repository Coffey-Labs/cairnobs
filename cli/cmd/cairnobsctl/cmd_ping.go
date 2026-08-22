package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// parsePingArgs resolves the api base URL for ping: --api flag wins, then
// $CAIRNOBSCTL_API_URL, then the hardcoded default. Kept pure (env passed in
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

	req, err := http.NewRequest(http.MethodGet, apiURL+"/healthz", nil)
	if err != nil {
		fmt.Fprintf(stderr, "building request: %v\n", err)
		return 1
	}
	setAuth(req, resolveToken(os.Getenv))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
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
