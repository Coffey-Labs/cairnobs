package main

import (
	"fmt"
	"io"
	"os"
)

func cmdAlerts(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "sentryctl alerts: expected a subcommand (list, get, apply)")
		return 1
	}
	alertingURL, rest := extractAlertingAPIFlag(args[1:], os.Getenv)

	switch args[0] {
	case "list":
		return httpGetJSON(alertingURL, "/rules", stdout, stderr)
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl alerts get: missing rule id")
			return 1
		}
		return httpGetJSON(alertingURL, "/rules/"+rest[0], stdout, stderr)
	case "apply":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl alerts apply: missing file path")
			return 1
		}
		// POST /rules accepts the same shape it returns -- a rule
		// definition file (query, condition, interval, notification
		// target ID) applies directly with no reshaping.
		return httpPostFileJSON(alertingURL, "/rules", rest[0], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "sentryctl alerts: unknown subcommand %q (want list, get, apply)\n", args[0])
		return 1
	}
}

func extractAlertingAPIFlag(args []string, env func(string) string) (alertingURL string, rest []string) {
	alertingURL = resolveAlertingURL(env)
	for i := 0; i < len(args); i++ {
		if args[i] == "--alerting-api" && i+1 < len(args) {
			alertingURL = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return alertingURL, rest
}
