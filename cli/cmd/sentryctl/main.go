// Command sentryctl is Sentry's control CLI. Six subcommands now
// (ping, query, dashboards, alerts) clearly justify splitting dispatch
// across files -- see cli/README.md's "revisit once there's a real
// command tree" note -- while keeping the same hand-rolled switch on
// os.Args, no CLI framework, per that same README.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

const (
	defaultAPIURL      = "http://localhost:8080"
	defaultAlertingURL = "http://localhost:8081"
)

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
	case "dashboards":
		return cmdDashboards(args[1:], stdout, stderr)
	case "alerts":
		return cmdAlerts(args[1:], stdout, stderr)
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
  sentryctl dashboards list|get <id>|apply <file> [--api <url>]
  sentryctl dashboards permissions list <dashboard-id> [--api <url>]
  sentryctl dashboards permissions grant <dashboard-id> <user-id> viewer|editor [--api <url>]
  sentryctl dashboards permissions revoke <dashboard-id> <user-id> [--api <url>]
  sentryctl alerts list|get <id>|apply <file> [--alerting-api <url>]

Commands:
  ping        Checks that the api service is reachable via GET /healthz.
  query       Runs a query (pipe syntax or SQL) against POST /query and
              prints the result as a table, or as JSON with --json. Quote
              the query in your shell -- pipe syntax uses "|", which your
              shell will otherwise interpret itself.
  dashboards  list/get/apply against api's dashboard CRUD endpoints.
              "apply <file>" imports a dashboard exported via the web
              UI's Export JSON button or GET /dashboards/{id}/export --
              the same JSON shape both places, Terraform-friendly.
              "permissions" grants/revokes/lists per-resource dashboard
              access (a Phase 4, enterprise-api-only feature -- a 501 on
              plain api means no enterprise permission service is wired
              in on this deployment, not a client error). A grant only
              ever raises someone to viewer or editor on one dashboard;
              Admin/Owner already have tenant-wide access.
  alerts      list/get/apply against alerting's rule CRUD endpoints.
              "apply <file>" creates a rule from a JSON file with the
              same shape POST /rules accepts.

--api defaults to $SENTRYCTL_API_URL, or `+defaultAPIURL+` if unset.
--alerting-api defaults to $SENTRYCTL_ALERTING_API_URL, or `+defaultAlertingURL+` if unset.
--language overrides auto-detection; omit it for the common case.

$SENTRYCTL_TOKEN, if set, is sent as "Authorization: Bearer <token>" on
every request -- required once a deployment configures enterprise-auth
(see /docs/phase-4-rbac-design.md). No flag equivalent, deliberately:
unlike --api, a credential shouldn't be typed where shell history or
`+"`ps`"+` output can capture it.`)
}

func resolveAPIURL(env func(string) string) string {
	if v := env("SENTRYCTL_API_URL"); v != "" {
		return v
	}
	return defaultAPIURL
}

func resolveAlertingURL(env func(string) string) string {
	if v := env("SENTRYCTL_ALERTING_API_URL"); v != "" {
		return v
	}
	return defaultAlertingURL
}

// resolveToken reads the RoleService/human bearer credential sentryctl
// presents to api/alerting once enterprise-auth enforcement is turned
// on (api/internal/authz.RequireRole*) -- empty by default, matching
// every other Phase 0-3 client's nil-authorizer no-op behavior.
func resolveToken(env func(string) string) string {
	return env("SENTRYCTL_TOKEN")
}

type errorResponseBody struct {
	Error string `json:"error"`
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
