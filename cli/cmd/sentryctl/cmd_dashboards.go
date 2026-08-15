package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func cmdDashboards(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "sentryctl dashboards: expected a subcommand (list, get, apply, permissions)")
		return 1
	}
	apiURL, rest := extractAPIFlag(args[1:], os.Getenv)
	token := resolveToken(os.Getenv)

	switch args[0] {
	case "list":
		return httpGetJSON(apiURL, "/dashboards", token, stdout, stderr)
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards get: missing dashboard id")
			return 1
		}
		return httpGetJSON(apiURL, "/dashboards/"+rest[0], token, stdout, stderr)
	case "apply":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards apply: missing file path")
			return 1
		}
		// The import endpoint consumes exactly the shape GET
		// /dashboards/{id}/export produces and the web UI's Export JSON
		// button downloads -- one JSON contract, three call sites.
		return httpPostFileJSON(apiURL, "/dashboards/import", token, rest[0], stdout, stderr)
	case "permissions":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards permissions: expected a subcommand (list, grant, revoke)")
			return 1
		}
		return cmdDashboardsPermissions(rest, apiURL, token, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "sentryctl dashboards: unknown subcommand %q (want list, get, apply, permissions)\n", args[0])
		return 1
	}
}

// cmdDashboardsPermissions is api/dashboards.PermissionStore's CLI
// surface -- PUT/DELETE /dashboards/{id}/permissions/{userId} existed
// with no caller but Go tests and curl until now (see
// /docs/phase-4-runbook.md's "Known gaps"). Kept as dashboards'
// own sub-subcommand rather than a flat sentryctl command (like
// "sentryctl dashboard-permissions grant ...") since a grant only ever
// makes sense in the context of one specific dashboard -- args[0]
// selects list/grant/revoke.
func cmdDashboardsPermissions(args []string, apiURL, token string, stdout, stderr io.Writer) int {
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "sentryctl dashboards permissions list: missing dashboard id")
			return 1
		}
		return httpGetJSON(apiURL, "/dashboards/"+rest[0]+"/permissions", token, stdout, stderr)
	case "grant":
		if len(rest) < 3 {
			fmt.Fprintln(stderr, "sentryctl dashboards permissions grant: usage: grant <dashboard-id> <user-id> <viewer|editor>")
			return 1
		}
		dashboardID, userID, role := rest[0], rest[1], rest[2]
		// Mirrors api/dashboards.validGrantRole -- Admin/Owner already
		// have tenant-wide dashboard access, so a resource-level grant
		// only ever raises someone as high as Editor; the server
		// rejects anything else too, this just fails faster/locally.
		if role != "viewer" && role != "editor" {
			fmt.Fprintf(stderr, "sentryctl dashboards permissions grant: role must be \"viewer\" or \"editor\", got %q\n", role)
			return 1
		}
		body := fmt.Sprintf(`{"role":%q}`, role)
		path := "/dashboards/" + dashboardID + "/permissions/" + userID
		return httpMutateNoBody(http.MethodPut, apiURL, path, token, body, "granted", stdout, stderr)
	case "revoke":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "sentryctl dashboards permissions revoke: usage: revoke <dashboard-id> <user-id>")
			return 1
		}
		dashboardID, userID := rest[0], rest[1]
		path := "/dashboards/" + dashboardID + "/permissions/" + userID
		return httpMutateNoBody(http.MethodDelete, apiURL, path, token, "", "revoked", stdout, stderr)
	default:
		fmt.Fprintf(stderr, "sentryctl dashboards permissions: unknown subcommand %q (want list, grant, revoke)\n", sub)
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
