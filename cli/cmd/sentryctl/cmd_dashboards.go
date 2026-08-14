package main

import (
	"fmt"
	"io"
	"os"
)

func cmdDashboards(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "sentryctl dashboards: expected a subcommand (list, get, apply)")
		return 1
	}
	apiURL, rest := extractAPIFlag(args[1:], os.Getenv)

	switch args[0] {
	case "list":
		return httpGetJSON(apiURL, "/dashboards", stdout, stderr)
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards get: missing dashboard id")
			return 1
		}
		return httpGetJSON(apiURL, "/dashboards/"+rest[0], stdout, stderr)
	case "apply":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards apply: missing file path")
			return 1
		}
		// The import endpoint consumes exactly the shape GET
		// /dashboards/{id}/export produces and the web UI's Export JSON
		// button downloads -- one JSON contract, three call sites.
		return httpPostFileJSON(apiURL, "/dashboards/import", rest[0], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "sentryctl dashboards: unknown subcommand %q (want list, get, apply)\n", args[0])
		return 1
	}
}

// extractAPIFlag pulls an optional --api <url> out of args, resolving
// the default the same way parsePingArgs/parseQueryArgs do, and returns
// the remaining positional args. Shared by dashboards and alerts since
// both take an optional --api/--alerting-api override the same way.
func extractAPIFlag(args []string, env func(string) string) (apiURL string, rest []string) {
	apiURL = resolveAPIURL(env)
	for i := 0; i < len(args); i++ {
		if args[i] == "--api" && i+1 < len(args) {
			apiURL = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return apiURL, rest
}
